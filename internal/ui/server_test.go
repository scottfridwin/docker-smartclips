package ui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scottfridwin/docker-smartclips/internal/config"
)

func newServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	media, dir := t.TempDir(), t.TempDir()
	data, err := os.ReadFile("../../testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(media, "Movies", "Sample (2024)"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "Movies", "Sample (2024)", "sample.mkv"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "clips.json")
	s := New(Options{ConfigPath: cfg, MediaRoot: media, StatusPath: filepath.Join(dir, "status.json")},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, cfg
}

func do(t *testing.T, method, url, body string, header map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp, out
}

var jsonHeader = map[string]string{"Content-Type": "application/json"}

func TestClipsRoundTrip(t *testing.T) {
	srv, cfg := newServer(t)
	resp, got := do(t, "GET", srv.URL+"/api/clips", "", nil)
	if resp.StatusCode != 200 || len(got["clips"].([]any)) != 0 {
		t.Fatalf("initial GET: %d %v", resp.StatusCode, got)
	}
	version := got["version"].(string)
	body := `{"version":"` + version + `","clips":[{"input":"Movies/Sample (2024)/sample.mkv","output":"Clip","start":2,"end":8}]}`
	resp, got = do(t, "PUT", srv.URL+"/api/clips", body, jsonHeader)
	if resp.StatusCode != 200 {
		t.Fatalf("PUT: %d %v", resp.StatusCode, got)
	}
	f, err := config.Load(cfg)
	if err != nil || len(f.Clips) != 1 || f.Version != got["version"] {
		t.Fatalf("saved file: %+v %v", f, err)
	}
	// Saving again with the stale version is a conflict.
	resp, _ = do(t, "PUT", srv.URL+"/api/clips", body, jsonHeader)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("stale version: %d", resp.StatusCode)
	}
}

func TestPutValidation(t *testing.T) {
	srv, _ := newServer(t)
	_, got := do(t, "GET", srv.URL+"/api/clips", "", nil)
	version := got["version"].(string)
	body := `{"version":"` + version + `","clips":[{"input":"../../etc/passwd","output":"a/b","start":5,"end":1}]}`
	resp, got := do(t, "PUT", srv.URL+"/api/clips", body, jsonHeader)
	if resp.StatusCode != http.StatusUnprocessableEntity || len(got["problems"].([]any)) < 3 {
		t.Errorf("invalid clip: %d %v", resp.StatusCode, got)
	}
}

func TestRequestGuards(t *testing.T) {
	srv, _ := newServer(t)
	resp, _ := do(t, "PUT", srv.URL+"/api/clips", `{}`, map[string]string{"Content-Type": "text/plain"})
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain PUT: %d", resp.StatusCode)
	}
	resp, _ = do(t, "PUT", srv.URL+"/api/clips", `{}`, map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site PUT: %d", resp.StatusCode)
	}
	resp, _ = do(t, "GET", srv.URL+"/", "", nil)
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("security headers: %v", resp.Header)
	}
	for _, asset := range []string{"/", "/app.js", "/app.css", "/favicon.svg"} {
		if resp, _ := do(t, "GET", srv.URL+asset, "", nil); resp.StatusCode != 200 {
			t.Errorf("%s: %d", asset, resp.StatusCode)
		}
	}
}

func TestBrowseIsConfined(t *testing.T) {
	srv, _ := newServer(t)
	_, got := do(t, "GET", srv.URL+"/api/browse?path=Movies", "", nil)
	entries := got["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["name"] != "Sample (2024)" {
		t.Errorf("browse Movies: %v", got)
	}
	// Traversal is clamped to the media root rather than escaping it.
	_, got = do(t, "GET", srv.URL+"/api/browse?path=../../..", "", nil)
	if got["path"] != "" {
		t.Errorf("traversal path: %v", got)
	}
	names := []string{}
	for _, e := range got["entries"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "Movies" {
		t.Errorf("traversal listed %v", names)
	}
}

func TestProbe(t *testing.T) {
	srv, _ := newServer(t)
	resp, got := do(t, "GET", srv.URL+"/api/probe?path="+strings.ReplaceAll("Movies/Sample (2024)/sample.mkv", " ", "%20"), "", nil)
	if resp.StatusCode != 200 || got["byte_range"] != true || len(got["keyframes"].([]any)) != 15 {
		t.Errorf("probe: %d %v", resp.StatusCode, got)
	}
	resp, _ = do(t, "GET", srv.URL+"/api/probe?path=/etc/passwd", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("probe outside media: %d", resp.StatusCode)
	}
	resp, _ = do(t, "GET", srv.URL+"/api/frame?path=/etc/passwd&t=1", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("frame outside media: %d", resp.StatusCode)
	}
}

func TestFrameAndPreview(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	srv, _ := newServer(t)
	p := strings.ReplaceAll("Movies/Sample (2024)/sample.mkv", " ", "%20")
	resp, err := http.Get(srv.URL + "/api/frame?path=" + p + "&t=4")
	if err != nil {
		t.Fatal(err)
	}
	img, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" || len(img) < 100 || img[0] != 0xFF || img[1] != 0xD8 {
		t.Errorf("frame: %d %s %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(img))
	}
	resp, err = http.Get(srv.URL + "/api/preview?path=" + p + "&start=4&dur=3")
	if err != nil {
		t.Fatal(err)
	}
	video, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp4" || len(video) < 1000 {
		t.Errorf("preview: %d %s %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(video))
	}
	if resp, _ := do(t, "GET", srv.URL+"/api/preview?path="+p+"&start=4&dur=600", "", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("long preview: %d", resp.StatusCode)
	}
}
