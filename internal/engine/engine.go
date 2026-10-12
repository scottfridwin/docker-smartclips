// Package engine turns clip definitions into ready-to-serve layouts, persisted in an index.
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/scottfridwin/docker-smartclips/internal/config"
	"github.com/scottfridwin/docker-smartclips/internal/layout"
	"github.com/scottfridwin/docker-smartclips/internal/mkv"
	"github.com/scottfridwin/docker-smartclips/internal/nfo"
)

// Sink receives the clips that are ready to serve.
type Sink interface {
	// SetClip publishes path (no extension) with its layout and optional NFO.
	SetClip(path string, l *layout.Layout, nfo []byte, nfoTime time.Time)
	RemoveClip(path string)
}

// Options configures an Engine.
type Options struct {
	MediaRoot     string
	CacheDir      string
	FFmpeg        string
	Workers       int
	RemuxTimeout  time.Duration
	RecheckPeriod time.Duration
}

// States reported in the status file.
const (
	StatePending = "pending"
	StateReady   = "ready"
	StateError   = "error"
)

// Status is one clip's state, as written to status.json for the UI.
type Status struct {
	State       string  `json:"state"`
	Kind        string  `json:"kind,omitempty"`
	Error       string  `json:"error,omitempty"`
	ActualStart float64 `json:"actual_start"`
	ActualEnd   float64 `json:"actual_end"`
	Size        int64   `json:"size,omitempty"`
}

type clipState struct {
	clip   config.Clip
	input  string
	key    string
	status Status
	ready  *layout.Layout
}

// Engine tracks the configured clips and keeps the Sink in sync with them.
type Engine struct {
	opt  Options
	sink Sink
	log  *slog.Logger

	mu       sync.Mutex
	clips    map[string]*clipState // by clip path
	file     *config.File
	queue    chan string // clip paths to build
	queued   map[string]bool
	building int
	started  bool
	dirty    bool
}

// New returns an engine that publishes to sink.
func New(opt Options, sink Sink, log *slog.Logger) *Engine {
	if opt.Workers <= 0 {
		opt.Workers = 2
	}
	if opt.FFmpeg == "" {
		opt.FFmpeg = "ffmpeg"
	}
	if opt.RemuxTimeout <= 0 {
		opt.RemuxTimeout = 30 * time.Minute
	}
	return &Engine{
		opt: opt, sink: sink, log: log,
		clips:  map[string]*clipState{},
		queue:  make(chan string, 1<<16),
		queued: map[string]bool{},
	}
}

func (e *Engine) indexPath(key string) string {
	return filepath.Join(e.opt.CacheDir, "index", key+".json")
}
func (e *Engine) remuxPath(key string) string {
	return filepath.Join(e.opt.CacheDir, "remux", key+".mkv")
}

// StatusPath is where the status file is written.
func (e *Engine) StatusPath() string { return filepath.Join(e.opt.CacheDir, "status.json") }

func clipKey(c config.Clip, origin layout.FileID) string {
	b, _ := json.Marshal([]any{layout.FormatVersion, origin, c.Start, c.End, c.Output})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// Apply makes the engine match file: unchanged clips are kept, layouts already in the
// index are published immediately, and the rest are queued for the workers.
func (e *Engine) Apply(file *config.File) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.file = file
	// Clips that fail validation (unsafe names, paths outside the media folder, ...) are
	// reported but never mounted.
	problems := map[int][]string{}
	var ve *config.ValidationError
	if err := config.Validate(file.Clips, e.opt.MediaRoot); errors.As(err, &ve) {
		for _, p := range ve.Problems {
			problems[p.Index] = append(problems[p.Index], p.Message)
		}
	}
	want := make(map[string]config.Clip, len(file.Clips))
	invalid := map[string]string{}
	for i, c := range file.Clips {
		if msgs, bad := problems[i]; bad {
			invalid[c.Path()] = strings.Join(msgs, "; ")
			continue
		}
		want[c.Path()] = c
	}
	for path, st := range e.clips {
		if _, ok := want[path]; ok {
			continue
		}
		if st.ready != nil {
			e.sink.RemoveClip(path)
		}
		if _, ok := invalid[path]; !ok {
			delete(e.clips, path)
			e.log.Info("clip removed", "clip", path)
		}
	}
	for path, msg := range invalid {
		if _, ok := want[path]; ok {
			continue
		}
		if st := e.clips[path]; st == nil || st.status.Error != msg {
			e.log.Warn("clip invalid", "clip", path, "err", msg)
		}
		e.clips[path] = &clipState{status: Status{State: StateError, Error: msg}}
	}
	for path, c := range want {
		e.applyClip(path, c)
	}
	e.dirty = true
}

func (e *Engine) applyClip(path string, c config.Clip) {
	st := e.clips[path]
	if st == nil {
		st = &clipState{}
		e.clips[path] = st
	}
	fail := func(err error) {
		if st.ready != nil {
			e.sink.RemoveClip(path)
		}
		*st = clipState{clip: c, status: Status{State: StateError, Error: err.Error()}}
	}
	input, err := config.ResolveInput(e.opt.MediaRoot, c.Input)
	if err != nil {
		fail(fmt.Errorf("input %w", err))
		return
	}
	origin, err := layout.Stat(input)
	if err != nil {
		fail(err)
		return
	}
	key := clipKey(c, origin)
	nfoChanged := !metadataEqual(st.clip, c)
	if st.key == key && st.status.State != StateError && (st.ready == nil || e.dataValid(st.ready)) {
		st.clip = c
		if nfoChanged && st.ready != nil {
			e.publish(path, st)
		}
		return
	}
	if st.ready != nil && st.ready.Origin != origin {
		// The source was replaced: the old byte ranges are no longer valid.
		e.sink.RemoveClip(path)
		st.ready = nil
	}
	st.clip, st.input, st.key = c, input, key
	if l, err := layout.Load(e.indexPath(key)); err == nil && e.dataValid(l) {
		st.ready = l
		st.status = statusOf(l)
		e.publish(path, st)
		return
	}
	// Keep serving the previous version (if any) until the new one is built.
	st.status = Status{State: StatePending}
	e.enqueue(path)
}

func metadataEqual(a, b config.Clip) bool {
	x, _ := json.Marshal([]any{a.NfoType, a.Metadata})
	y, _ := json.Marshal([]any{b.NfoType, b.Metadata})
	return bytes.Equal(x, y)
}

// dataValid reports whether the file l's byte ranges refer to is unchanged.
func (e *Engine) dataValid(l *layout.Layout) bool {
	id, err := layout.Stat(l.Data.Path)
	return err == nil && id == l.Data
}

func statusOf(l *layout.Layout) Status {
	return Status{State: StateReady, Kind: l.Kind, ActualStart: l.ActualStart, ActualEnd: l.ActualEnd, Size: l.Size}
}

func (e *Engine) publish(path string, st *clipState) {
	var data []byte
	if st.clip.Metadata != nil || st.clip.NfoType != "" {
		data = nfo.Generate(st.clip.NfoType, st.clip.Metadata, config.ValidKey)
	}
	nfoTime := e.file.ModTime
	if nfoTime.IsZero() {
		nfoTime = time.Unix(0, st.ready.Built)
	}
	e.sink.SetClip(path, st.ready, data, nfoTime)
}

func (e *Engine) enqueue(path string) {
	if e.queued[path] {
		return
	}
	e.queued[path] = true
	e.queue <- path
}

// Start runs the workers and the periodic source check until ctx ends.
func (e *Engine) Start(ctx context.Context) {
	for _, dir := range []string{"index", "remux"} {
		if err := os.MkdirAll(filepath.Join(e.opt.CacheDir, dir), 0o755); err != nil {
			e.log.Error("cache directory", "err", err)
		}
	}
	// Temporary files left by an interrupted remux.
	if tmps, err := filepath.Glob(filepath.Join(e.opt.CacheDir, "remux", ".*.tmp.mkv")); err == nil {
		for _, t := range tmps {
			_ = os.Remove(t)
		}
	}
	e.mu.Lock()
	e.started = true
	e.mu.Unlock()
	for i := 0; i < e.opt.Workers; i++ {
		go e.worker(ctx)
	}
	go e.housekeeping(ctx)
}

func (e *Engine) housekeeping(ctx context.Context) {
	statusTick := time.NewTicker(time.Second)
	defer statusTick.Stop()
	var recheck <-chan time.Time
	if e.opt.RecheckPeriod > 0 {
		t := time.NewTicker(e.opt.RecheckPeriod)
		defer t.Stop()
		recheck = t.C
	}
	cleaned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-recheck:
			e.Recheck()
		case <-statusTick.C:
			e.mu.Lock()
			idle := len(e.queued) == 0 && e.building == 0
			if e.dirty {
				e.dirty = false
				if err := e.writeStatus(); err != nil {
					e.log.Warn("status file", "err", err)
				}
				cleaned = false
			}
			e.mu.Unlock()
			if idle && !cleaned {
				e.cleanCache()
				cleaned = true
			}
		}
	}
}

// Recheck re-applies the current file, picking up replaced source files.
func (e *Engine) Recheck() {
	e.mu.Lock()
	f := e.file
	e.mu.Unlock()
	if f != nil {
		e.Apply(f)
	}
}

func (e *Engine) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case path := <-e.queue:
			e.mu.Lock()
			delete(e.queued, path)
			st, ok := e.clips[path]
			if !ok {
				e.mu.Unlock()
				continue
			}
			c, input, key := st.clip, st.input, st.key
			e.building++
			e.mu.Unlock()

			t0 := time.Now()
			l, err := e.build(ctx, c, input, key)

			e.mu.Lock()
			e.building--
			e.dirty = true
			st, ok = e.clips[path]
			if !ok || st.key != key {
				e.mu.Unlock()
				continue // removed or changed while building
			}
			if err != nil {
				e.log.Error("clip failed", "clip", path, "err", err)
				if st.ready != nil {
					e.sink.RemoveClip(path)
					st.ready = nil
				}
				st.status = Status{State: StateError, Error: err.Error()}
			} else {
				e.log.Info("clip ready", "clip", path, "kind", l.Kind, "size", l.Size,
					"actual_start", fmt.Sprintf("%.3f", l.ActualStart), "took", time.Since(t0).Round(time.Millisecond))
				st.ready = l
				st.status = statusOf(l)
				e.publish(path, st)
			}
			e.mu.Unlock()
		}
	}
}

var matroskaExt = map[string]bool{".mkv": true, ".mk3d": true, ".webm": true}

func (e *Engine) build(ctx context.Context, c config.Clip, input, key string) (*layout.Layout, error) {
	f, err := os.Open(input)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	origin, err := layout.Stat(input)
	if err != nil {
		return nil, err
	}
	if clipKey(c, origin) != key {
		return nil, errors.New("source changed while building; will retry")
	}
	l := &layout.Layout{Version: layout.FormatVersion, Origin: origin, Start: c.Start, End: c.End, Built: time.Now().UnixNano()}
	if matroskaExt[strings.ToLower(filepath.Ext(input))] {
		clip, err := buildMKV(f, origin.Size, c)
		if err == nil {
			l.Kind, l.Data, l.Parts = layout.KindMKV, origin, clip.Parts
			l.ActualStart, l.ActualEnd = clip.ActualStart, clip.ActualEnd
			if err := l.Finalize(); err != nil {
				return nil, err
			}
			return l, l.Save(e.indexPath(key))
		}
		e.log.Warn("byte-range clip not possible; remuxing instead", "clip", c.Path(), "reason", err)
	}
	return e.remux(ctx, c, input, key, l)
}

func buildMKV(f *os.File, size int64, c config.Clip) (*mkv.Clip, error) {
	src, err := mkv.Open(f, size)
	if err != nil {
		return nil, err
	}
	return src.BuildClip(f, c.Start, c.End, c.Output)
}

func (e *Engine) remux(ctx context.Context, c config.Clip, input, key string, l *layout.Layout) (*layout.Layout, error) {
	out := e.remuxPath(key)
	tmp := filepath.Join(filepath.Dir(out), "."+key+".tmp.mkv")
	defer func() { _ = os.Remove(tmp) }()
	ctx, cancel := context.WithTimeout(ctx, e.opt.RemuxTimeout)
	defer cancel()
	run := func(subs bool) error {
		args := []string{"-nostdin", "-hide_banner", "-loglevel", "error",
			"-ss", fmt.Sprintf("%.3f", c.Start), "-i", input,
			"-t", fmt.Sprintf("%.3f", c.End-c.Start),
			"-map", "0:v", "-map", "0:a?"}
		if subs {
			args = append(args, "-map", "0:s?")
		}
		args = append(args, "-c", "copy", "-avoid_negative_ts", "make_zero",
			"-metadata", "title="+c.Output, "-f", "matroska", "-y", tmp)
		cmd := exec.CommandContext(ctx, e.opt.FFmpeg, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	if err := run(true); err != nil {
		// Some subtitle formats (e.g. mov_text) can't be stream-copied into Matroska.
		e.log.Warn("remux with subtitles failed; retrying without", "clip", c.Path(), "err", err)
		if err := run(false); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(tmp, out); err != nil {
		return nil, err
	}
	data, err := layout.Stat(out)
	if err != nil {
		return nil, err
	}
	l.Kind, l.Data = layout.KindRemux, data
	l.ActualStart, l.ActualEnd = c.Start, c.End
	l.Parts = []layout.Part{layout.Range(0, data.Size)}
	if err := l.Finalize(); err != nil {
		return nil, err
	}
	return l, l.Save(e.indexPath(key))
}

// Invalidate is called when a served clip's data file no longer matches its layout.
func (e *Engine) Invalidate(path string) {
	e.log.Warn("clip data changed underneath; rebuilding", "clip", path)
	go e.Recheck()
}

func (e *Engine) writeStatus() error {
	if !e.started {
		return nil
	}
	out := make(map[string]Status, len(e.clips))
	for path, st := range e.clips {
		out[path] = st.status
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return layout.WriteFileAtomic(e.StatusPath(), b, 0o644)
}

// cleanCache deletes index entries and remuxed files no current clip uses.
func (e *Engine) cleanCache() {
	e.mu.Lock()
	keep := make(map[string]bool, len(e.clips))
	for _, st := range e.clips {
		keep[st.key] = true
	}
	e.mu.Unlock()
	for _, dir := range []string{"index", "remux"} {
		entries, err := os.ReadDir(filepath.Join(e.opt.CacheDir, dir))
		if err != nil {
			continue
		}
		for _, ent := range entries {
			name := ent.Name()
			key := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".mkv")
			if strings.HasPrefix(name, ".") || len(key) != 32 || keep[key] {
				continue
			}
			if err := os.Remove(filepath.Join(e.opt.CacheDir, dir, name)); err == nil {
				e.log.Info("cache entry removed", "file", filepath.Join(dir, name))
			}
		}
	}
}
