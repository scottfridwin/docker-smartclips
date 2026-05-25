package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type Clip struct {
	Input    string                 `json:"input"`
	Output   string                 `json:"output"`
	Start    float64                `json:"start"`
	End      float64                `json:"end"`
	Group    string                 `json:"group,omitempty"`
	NfoType  string                 `json:"nfo_type,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

var (
	configPath string
	mediaPath  string
	mu         sync.RWMutex
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadClips() ([]Clip, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var clips []Clip
	return clips, json.Unmarshal(data, &clips)
}

func saveClips(clips []Clip) error {
	data, err := json.MarshalIndent(clips, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

func apiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		clips, err := loadClips()
		mu.RUnlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(clips)

	case http.MethodPut:
		mu.Lock()
		defer mu.Unlock()
		var clips []Clip
		if err := json.NewDecoder(r.Body).Decode(&clips); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := saveClips(clips); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// resolveMediaFile resolves an input path to a real file on disk.
// Supports both absolute paths (within the media mount) and relative paths.
func resolveMediaFile(input string) (string, error) {
	var resolved string
	if filepath.IsAbs(input) {
		resolved = input
	} else {
		resolved = filepath.Join(mediaPath, input)
	}
	resolved = filepath.Clean(resolved)

	// Security: ensure the resolved path is under mediaPath
	if !strings.HasPrefix(resolved, filepath.Clean(mediaPath)+"/") && resolved != filepath.Clean(mediaPath) {
		return "", fmt.Errorf("access denied")
	}

	if _, err := os.Stat(resolved); err != nil {
		return "", fmt.Errorf("file not found: %s", input)
	}
	return resolved, nil
}

// mediaHandler serves media files for in-browser playback with range request support.
func mediaHandler(w http.ResponseWriter, r *http.Request) {
	input := r.URL.Query().Get("path")
	if input == "" {
		http.Error(w, "missing 'path' parameter", http.StatusBadRequest)
		return
	}

	resolved, err := resolveMediaFile(input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	http.ServeFile(w, r, resolved)
}

// probeHandler returns media duration and basic info via ffprobe.
func probeHandler(w http.ResponseWriter, r *http.Request) {
	input := r.URL.Query().Get("path")
	if input == "" {
		http.Error(w, "missing 'path' parameter", http.StatusBadRequest)
		return
	}

	resolved, err := resolveMediaFile(input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	cmd := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		resolved,
	)
	out, err := cmd.Output()
	if err != nil {
		http.Error(w, "ffprobe failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// previewHandler generates a short clip preview via ffmpeg and streams it back.
func previewHandler(w http.ResponseWriter, r *http.Request) {
	input := r.URL.Query().Get("path")
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")

	if input == "" || startStr == "" || endStr == "" {
		http.Error(w, "missing 'path', 'start', or 'end' parameter", http.StatusBadRequest)
		return
	}

	start, err := strconv.ParseFloat(startStr, 64)
	if err != nil {
		http.Error(w, "invalid start time", http.StatusBadRequest)
		return
	}
	end, err := strconv.ParseFloat(endStr, 64)
	if err != nil {
		http.Error(w, "invalid end time", http.StatusBadRequest)
		return
	}
	if start >= end {
		http.Error(w, "start must be less than end", http.StatusBadRequest)
		return
	}

	resolved, err := resolveMediaFile(input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	duration := end - start

	cmd := exec.Command("ffmpeg",
		"-ss", fmt.Sprintf("%.3f", start),
		"-i", resolved,
		"-t", fmt.Sprintf("%.3f", duration),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28",
		"-c:a", "aac", "-b:a", "128k",
		"-movflags", "frag_keyframe+empty_moov+faststart",
		"-f", "mp4",
		"pipe:1",
	)

	w.Header().Set("Content-Type", "video/mp4")
	cmd.Stdout = w

	if err := cmd.Run(); err != nil {
		log.Printf("preview ffmpeg error: %v", err)
	}
}

func main() {
	configPath = getEnv("SMARTCLIPS_CONFIG", "/config/clips.json")
	mediaPath = getEnv("SMARTCLIPS_MEDIA", "/media")
	listenAddr := getEnv("SMARTCLIPS_UI_LISTEN", ":8080")
	staticDir := getEnv("SMARTCLIPS_UI_STATIC", "/app/static")

	http.HandleFunc("/api/clips", apiHandler)
	http.HandleFunc("/api/media", mediaHandler)
	http.HandleFunc("/api/probe", probeHandler)
	http.HandleFunc("/api/preview", previewHandler)
	http.Handle("/", http.FileServer(http.Dir(staticDir)))

	log.Printf("SmartClips UI listening on %s (config: %s, media: %s)", listenAddr, configPath, mediaPath)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
