package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClipFullPath(t *testing.T) {
	tests := []struct {
		name   string
		clip   Clip
		expect string
	}{
		{"with group", Clip{Group: "Show/Season 01", Output: "Cold Open"}, "Show/Season 01/Cold Open"},
		{"without group", Clip{Output: "Blooper Reel"}, "Blooper Reel"},
		{"empty output", Clip{Group: "G"}, "G/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.clip.FullPath(); got != tt.expect {
				t.Errorf("FullPath() = %q, want %q", got, tt.expect)
			}
		})
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "clips.json")
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoadValid(t *testing.T) {
	path := writeTemp(t, `[
		{"input":"/media/a.mkv","output":"Clip A","start":0,"end":10},
		{"input":"relative.mkv","output":"Clip B","start":5,"end":20,"group":"G"}
	]`)

	clips, err := Load(path, "/prefix")
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) != 2 {
		t.Fatalf("expected 2 clips, got %d", len(clips))
	}
	// Absolute path unchanged
	if clips[0].Input != "/media/a.mkv" {
		t.Errorf("absolute path modified: %q", clips[0].Input)
	}
	// Relative path gets prefix
	if clips[1].Input != filepath.Join("/prefix", "relative.mkv") {
		t.Errorf("relative path not prefixed: %q", clips[1].Input)
	}
}

func TestLoadDuplicatePath(t *testing.T) {
	path := writeTemp(t, `[
		{"input":"/a.mkv","output":"X","start":0,"end":10,"group":"G"},
		{"input":"/b.mkv","output":"X","start":0,"end":10,"group":"G"}
	]`)

	_, err := Load(path, "")
	if err == nil {
		t.Fatal("expected error for duplicate path")
	}
}

func TestLoadInvalidTimes(t *testing.T) {
	path := writeTemp(t, `[{"input":"/a.mkv","output":"X","start":10,"end":5}]`)

	_, err := Load(path, "")
	if err == nil {
		t.Fatal("expected error for start >= end")
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	path := writeTemp(t, `not json`)

	_, err := Load(path, "")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path.json", "")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
