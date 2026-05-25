package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func setupTestConfig(t *testing.T, initial []Clip) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clips.json")
	data, _ := json.Marshal(initial)
	os.WriteFile(path, data, 0644)
	configPath = path
	return path
}

func TestGetClips(t *testing.T) {
	clips := []Clip{
		{Input: "/media/a.mkv", Output: "A", Start: 0, End: 10},
		{Input: "/media/b.mkv", Output: "B", Start: 5, End: 20, Group: "G"},
	}
	setupTestConfig(t, clips)

	req := httptest.NewRequest(http.MethodGet, "/api/clips", nil)
	w := httptest.NewRecorder()
	apiHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var got []Clip
	json.NewDecoder(w.Body).Decode(&got)
	if len(got) != 2 {
		t.Fatalf("expected 2 clips, got %d", len(got))
	}
	if got[0].Output != "A" || got[1].Group != "G" {
		t.Errorf("unexpected clip data: %+v", got)
	}
}

func TestPutClips(t *testing.T) {
	setupTestConfig(t, []Clip{})

	newClips := []Clip{
		{Input: "/media/x.mkv", Output: "X", Start: 1, End: 5},
	}
	body, _ := json.Marshal(newClips)
	req := httptest.NewRequest(http.MethodPut, "/api/clips", bytes.NewReader(body))
	w := httptest.NewRecorder()
	apiHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify file was written
	data, _ := os.ReadFile(configPath)
	var saved []Clip
	json.Unmarshal(data, &saved)
	if len(saved) != 1 || saved[0].Output != "X" {
		t.Errorf("unexpected saved data: %+v", saved)
	}
}

func TestPutInvalidJSON(t *testing.T) {
	setupTestConfig(t, []Clip{})

	req := httptest.NewRequest(http.MethodPut, "/api/clips", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	apiHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	setupTestConfig(t, []Clip{})

	req := httptest.NewRequest(http.MethodDelete, "/api/clips", nil)
	w := httptest.NewRecorder()
	apiHandler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestRoundTrip(t *testing.T) {
	original := []Clip{
		{Input: "/m.mkv", Output: "C", Start: 1.5, End: 3.7, Group: "G", NfoType: "movie",
			Metadata: map[string]interface{}{"title": "Test"}},
	}
	setupTestConfig(t, original)

	// PUT
	body, _ := json.Marshal(original)
	req := httptest.NewRequest(http.MethodPut, "/api/clips", bytes.NewReader(body))
	w := httptest.NewRecorder()
	apiHandler(w, req)

	// GET
	req = httptest.NewRequest(http.MethodGet, "/api/clips", nil)
	w = httptest.NewRecorder()
	apiHandler(w, req)

	var got []Clip
	json.NewDecoder(w.Body).Decode(&got)
	if len(got) != 1 || got[0].Output != "C" || got[0].Start != 1.5 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}
