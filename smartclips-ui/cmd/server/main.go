package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
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

func main() {
	configPath = getEnv("SMARTCLIPS_CONFIG", "/config/clips.json")
	listenAddr := getEnv("SMARTCLIPS_UI_LISTEN", ":8080")
	staticDir := getEnv("SMARTCLIPS_UI_STATIC", "/app/static")

	http.HandleFunc("/api/clips", apiHandler)
	http.Handle("/", http.FileServer(http.Dir(staticDir)))

	log.Printf("SmartClips UI listening on %s (config: %s)", listenAddr, configPath)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
