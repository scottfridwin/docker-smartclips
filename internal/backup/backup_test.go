package backup

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOnceCopiesChangesAndPrunes(t *testing.T) {
	src := filepath.Join(t.TempDir(), "clips.json")
	b := &Backup{Src: src, Dir: t.TempDir(), Keep: 2, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

	if err := b.Once(now); err != nil {
		t.Fatalf("missing clips file: %v", err)
	}
	if st := b.Status(); st.Copies != 0 {
		t.Fatalf("copied a missing file: %+v", st)
	}
	for i, content := range []string{"[1]", "[1]", "[2]", "[3]"} {
		if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := b.Once(now.Add(time.Duration(i) * time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	names, err := b.copies()
	if err != nil {
		t.Fatal(err)
	}
	// "[1]" twice is one copy; keeping 2 drops it.
	want := []string{"clips-20261012T110000Z.json", "clips-20261012T100000Z.json"}
	if len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("copies %v, want %v", names, want)
	}
	if got, _ := os.ReadFile(filepath.Join(b.Dir, names[0])); string(got) != "[3]" {
		t.Errorf("newest copy holds %q", got)
	}
	if st := b.Status(); st.Copies != 2 || !st.Latest.Equal(now.Add(3*time.Hour)) {
		t.Errorf("status %+v", st)
	}
}

func TestOnceLeavesOtherFiles(t *testing.T) {
	src := filepath.Join(t.TempDir(), "clips.json")
	dir := t.TempDir()
	other := filepath.Join(dir, "clips-notes.json")
	if err := os.WriteFile(other, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Backup{Src: src, Dir: dir, Keep: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(src, []byte{byte('a' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := b.Once(time.Now().Add(time.Duration(i) * time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("pruning removed a file it didn't write: %v", err)
	}
}

func TestOnceReportsUnwritableDir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "clips.json")
	if err := os.WriteFile(src, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Backup{Src: src, Dir: filepath.Join(t.TempDir(), "missing"), Keep: 3, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := b.Once(time.Now()); err == nil {
		t.Error("no error for a missing backup directory")
	}
}
