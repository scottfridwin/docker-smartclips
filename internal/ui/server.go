// Package ui serves the clip manager web interface and its JSON API.
package ui

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scottfridwin/docker-smartclips/internal/backup"
	"github.com/scottfridwin/docker-smartclips/internal/config"
	"github.com/scottfridwin/docker-smartclips/internal/mkv"
)

//go:embed static
var static embed.FS

// Options configures the server.
type Options struct {
	ConfigPath string
	MediaRoot  string
	StatusPath string
	FFmpeg     string
	FFprobe    string
	Backup     *backup.Backup // nil when backups are off
}

// Server is the UI HTTP handler.
type Server struct {
	opt  Options
	log  *slog.Logger
	mux  *http.ServeMux
	save sync.Mutex
	jobs chan struct{} // bounds concurrent ffmpeg processes
}

// New builds the handler.
func New(opt Options, log *slog.Logger) *Server {
	if opt.FFmpeg == "" {
		opt.FFmpeg = "ffmpeg"
	}
	if opt.FFprobe == "" {
		opt.FFprobe = "ffprobe"
	}
	s := &Server{opt: opt, log: log, mux: http.NewServeMux(), jobs: make(chan struct{}, 2)}
	sub, _ := fs.Sub(static, "static")
	s.mux.Handle("GET /", http.FileServer(http.FS(sub)))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	s.mux.HandleFunc("GET /api/clips", s.getClips)
	s.mux.HandleFunc("PUT /api/clips", s.putClips)
	s.mux.HandleFunc("GET /api/browse", s.browse)
	s.mux.HandleFunc("GET /api/probe", s.probe)
	s.mux.HandleFunc("GET /api/frame", s.frame)
	s.mux.HandleFunc("GET /api/preview", s.preview)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; media-src 'self' blob:; "+
		"style-src 'self'; script-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// Cross-site requests can't set a JSON content type without a CORS preflight, which
		// this server never answers; Sec-Fetch-Site rejects them outright in modern browsers.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			writeError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type clipsResponse struct {
	Clips    []config.Clip   `json:"clips"`
	Version  string          `json:"version"`
	Status   json.RawMessage `json:"status"`
	NfoTypes []string        `json:"nfo_types"`
	Backup   *backup.Status  `json:"backup,omitempty"`
}

func (s *Server) getClips(w http.ResponseWriter, _ *http.Request) {
	f, err := config.Load(s.opt.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status, err := os.ReadFile(s.opt.StatusPath)
	if err != nil || !json.Valid(status) {
		status = []byte("{}")
	}
	if f.Clips == nil {
		f.Clips = []config.Clip{}
	}
	resp := clipsResponse{Clips: f.Clips, Version: f.Version, Status: status, NfoTypes: config.NfoTypes}
	if s.opt.Backup != nil {
		st := s.opt.Backup.Status()
		resp.Backup = &st
	}
	writeJSON(w, http.StatusOK, resp)
}

type putRequest struct {
	Clips   []config.Clip `json:"clips"`
	Version string        `json:"version"`
}

func (s *Server) putClips(w http.ResponseWriter, r *http.Request) {
	var req putRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := config.Validate(req.Clips, s.opt.MediaRoot); err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "problems": ve.Problems})
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.save.Lock()
	defer s.save.Unlock()
	cur, err := config.Load(s.opt.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Version != cur.Version {
		writeError(w, http.StatusConflict, "the clips were changed elsewhere; reload and reapply your edits")
		return
	}
	version, err := config.Save(s.opt.ConfigPath, req.Clips)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("clips saved", "count", len(req.Clips), "version", version)
	writeJSON(w, http.StatusOK, map[string]string{"version": version})
}

type browseEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

// mediaDir resolves a relative directory inside the media root.
func (s *Server) mediaDir(rel string) (string, error) {
	root, err := filepath.EvalSymlinks(s.opt.MediaRoot)
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, filepath.Clean("/"+rel))
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.Rel(root, resolved); err != nil || r == ".." || strings.HasPrefix(r, "../") {
		return "", errors.New("outside the media folder")
	}
	return resolved, nil
}

func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	rel := strings.Trim(filepath.ToSlash(filepath.Clean("/"+r.URL.Query().Get("path"))), "/")
	dir, err := s.mediaDir(rel)
	if err != nil {
		writeError(w, http.StatusNotFound, "folder not found")
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, http.StatusNotFound, "folder not readable")
		return
	}
	out := []browseEntry{}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, name)) // follows symlinks
		if err != nil {
			continue
		}
		if info.IsDir() {
			out = append(out, browseEntry{Name: name, Dir: true})
		} else if config.VideoExts[strings.ToLower(filepath.Ext(name))] {
			out = append(out, browseEntry{Name: name, Size: info.Size()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "entries": out})
}

type probeResponse struct {
	Duration  float64   `json:"duration"`
	Keyframes []float64 `json:"keyframes"`
	ByteRange bool      `json:"byte_range"` // clips are served by reference (no copy)
	Note      string    `json:"note,omitempty"`
	// Resolved is set when the input's file is gone and its upgrade is used instead.
	Resolved string `json:"resolved,omitempty"`
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	input := r.URL.Query().Get("path")
	p, err := config.ResolveInput(s.opt.MediaRoot, input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "input "+err.Error())
		return
	}
	resp := probeResponse{Keyframes: []float64{}}
	if filepath.Base(p) != filepath.Base(input) {
		resp.Resolved = config.Relative(s.opt.MediaRoot, p)
	}
	if ext := strings.ToLower(filepath.Ext(p)); ext == ".mkv" || ext == ".webm" || ext == ".mk3d" {
		if f, err := os.Open(p); err == nil {
			info, _ := f.Stat()
			src, err := mkv.Open(f, info.Size())
			_ = f.Close()
			if err == nil {
				resp.Duration, resp.Keyframes, resp.ByteRange = src.DurationSeconds(), src.Keyframes(), true
			} else {
				resp.Note = err.Error()
			}
		}
	}
	if resp.Duration == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, s.opt.FFprobe, "-v", "error", "-show_entries", "format=duration",
			"-of", "default=nw=1:nk=1", p).Output()
		if err != nil {
			writeError(w, http.StatusBadRequest, "can't read this file as video")
			return
		}
		resp.Duration, _ = strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		if resp.Note == "" {
			resp.Note = "not Matroska: the clip will be copied into the cache (uses disk space)"
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseSeconds(v string, limit float64) (float64, error) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || f < 0 || f > limit {
		return 0, fmt.Errorf("invalid time %q", v)
	}
	return f, nil
}

// run executes ffmpeg with bounded concurrency and returns its stdout.
func (s *Server) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	select {
	case s.jobs <- struct{}{}:
		defer func() { <-s.jobs }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.opt.FFmpeg, append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (s *Server) frame(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := config.ResolveInput(s.opt.MediaRoot, q.Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "input "+err.Error())
		return
	}
	t, err := parseSeconds(q.Get("t"), 48*3600)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	img, err := s.run(r.Context(), 30*time.Second, "-ss", fmt.Sprintf("%.3f", t), "-i", p,
		"-frames:v", "1", "-vf", "scale=640:-2:out_range=full,format=yuvj420p",
		"-f", "image2", "-c:v", "mjpeg", "-q:v", "5", "pipe:1")
	if err == nil && len(img) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no frame at this time (is it past the end of the video?)")
		return
	}
	if err != nil {
		s.log.Warn("frame failed", "err", err)
		writeError(w, http.StatusInternalServerError, "couldn't extract a frame")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(img)
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := config.ResolveInput(s.opt.MediaRoot, q.Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "input "+err.Error())
		return
	}
	start, err := parseSeconds(q.Get("start"), 48*3600)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dur, err := parseSeconds(q.Get("dur"), 30)
	if err != nil || dur < 1 {
		writeError(w, http.StatusBadRequest, "dur must be 1-30 seconds")
		return
	}
	tmp, err := os.CreateTemp("", "preview-*.mp4")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = s.run(r.Context(), 2*time.Minute, "-ss", fmt.Sprintf("%.3f", start), "-i", p,
		"-t", fmt.Sprintf("%.3f", dur), "-map", "0:v:0", "-map", "0:a:0?",
		"-vf", "scale=-2:480", "-c:v", "libx264", "-preset", "veryfast", "-crf", "28", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-ac", "2", "-b:a", "128k", "-movflags", "+faststart", "-f", "mp4", "-y", tmp.Name())
	var video []byte
	if err == nil {
		video, err = os.ReadFile(tmp.Name())
	}
	if err != nil {
		s.log.Warn("preview failed", "err", err)
		writeError(w, http.StatusInternalServerError, "couldn't build a preview")
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "preview.mp4", time.Time{}, bytes.NewReader(video))
}
