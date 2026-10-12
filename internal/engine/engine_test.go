package engine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/scottfridwin/docker-smartclips/internal/config"
	"github.com/scottfridwin/docker-smartclips/internal/layout"
)

type fakeSink struct {
	mu    sync.Mutex
	clips map[string]*layout.Layout
	nfos  map[string][]byte
}

func newSink() *fakeSink {
	return &fakeSink{clips: map[string]*layout.Layout{}, nfos: map[string][]byte{}}
}

func (s *fakeSink) SetClip(path string, l *layout.Layout, nfo []byte, _ time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clips[path], s.nfos[path] = l, nfo
}

func (s *fakeSink) RemoveClip(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clips, path)
	delete(s.nfos, path)
}

func (s *fakeSink) get(path string) *layout.Layout {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clips[path]
}

func (s *fakeSink) waitFor(t *testing.T, path string, cond func(*layout.Layout) bool) *layout.Layout {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if l := s.get(path); cond(l) {
			return l
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return nil
}

type env struct {
	media, cache string
}

func setup(t *testing.T) env {
	t.Helper()
	e := env{media: t.TempDir(), cache: t.TempDir()}
	data, err := os.ReadFile("../../testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.media, "sample.mkv"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e env) engine(sink Sink) *Engine {
	return New(Options{MediaRoot: e.media, CacheDir: e.cache, Workers: 2}, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func file(clips ...config.Clip) *config.File {
	return &config.File{Clips: clips, ModTime: time.Unix(1700000000, 0)}
}

var clipA = config.Clip{Input: "sample.mkv", Output: "A", Group: "G", Start: 5, End: 12,
	NfoType: "musicvideo", Metadata: map[string]any{"title": "A"}}

func TestBuildPublishAndReuseIndex(t *testing.T) {
	e := setup(t)
	sink := newSink()
	eng := e.engine(sink)
	eng.Apply(file(clipA))
	if sink.get("G/A") != nil {
		t.Fatal("published before being built")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	l := sink.waitFor(t, "G/A", func(l *layout.Layout) bool { return l != nil })
	if l.Kind != layout.KindMKV || l.ActualStart > 5 || l.ActualStart < 3.9 || l.Size == 0 {
		t.Errorf("layout %+v", l)
	}
	if string(sink.nfos["G/A"]) == "" {
		t.Error("no NFO")
	}

	// The status file is written for the UI.
	deadline := time.Now().Add(5 * time.Second)
	var status map[string]Status
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(eng.StatusPath()); err == nil && json.Unmarshal(b, &status) == nil && status["G/A"].State == StateReady {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status["G/A"].State != StateReady {
		t.Errorf("status %+v", status)
	}

	// A new engine publishes from the index immediately, without building.
	sink2 := newSink()
	eng2 := e.engine(sink2)
	eng2.Apply(file(clipA))
	if got := sink2.get("G/A"); got == nil || got.Built != l.Built {
		t.Errorf("not reused from the index: %+v", got)
	}
}

func TestSourceReplacedIsRebuilt(t *testing.T) {
	e := setup(t)
	sink := newSink()
	eng := e.engine(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	eng.Apply(file(clipA))
	first := sink.waitFor(t, "G/A", func(l *layout.Layout) bool { return l != nil })

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(e.media, "sample.mkv"), later, later); err != nil {
		t.Fatal(err)
	}
	eng.Recheck()
	second := sink.waitFor(t, "G/A", func(l *layout.Layout) bool { return l != nil && l.Origin != first.Origin })
	if second.Origin.ModTime != later.UnixNano() {
		t.Errorf("rebuilt layout has origin %+v", second.Origin)
	}
}

func TestChangedRemovedAndInvalid(t *testing.T) {
	e := setup(t)
	sink := newSink()
	eng := e.engine(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	b := config.Clip{Input: "sample.mkv", Output: "B", Start: 1, End: 3}
	eng.Apply(file(clipA, b))
	sink.waitFor(t, "B", func(l *layout.Layout) bool { return l != nil })

	changed := clipA
	changed.Start, changed.End = 20, 25
	evil := config.Clip{Input: "../../etc/passwd", Output: "Evil", Start: 1, End: 3}
	eng.Apply(file(changed, evil))
	sink.waitFor(t, "B", func(l *layout.Layout) bool { return l == nil })
	sink.waitFor(t, "G/A", func(l *layout.Layout) bool { return l != nil && l.Start == 20 })
	if sink.get("Evil") != nil {
		t.Error("invalid clip was published")
	}
	eng.mu.Lock()
	st := eng.clips["Evil"]
	eng.mu.Unlock()
	if st == nil || st.status.State != StateError {
		t.Errorf("invalid clip status %+v", st)
	}
}

func TestRemuxNonMatroska(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	e := setup(t)
	mp4 := filepath.Join(e.media, "sample.mp4")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-i", filepath.Join(e.media, "sample.mkv"),
		"-map", "0:v", "-map", "0:a", "-c", "copy", "-tag:v", "hvc1", mp4).CombinedOutput(); err != nil {
		t.Fatalf("making mp4: %v %s", err, out)
	}
	sink := newSink()
	eng := e.engine(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	eng.Apply(file(config.Clip{Input: "sample.mp4", Output: "M", Start: 3, End: 9}))
	l := sink.waitFor(t, "M", func(l *layout.Layout) bool { return l != nil })
	if l.Kind != layout.KindRemux || filepath.Dir(l.Data.Path) != filepath.Join(e.cache, "remux") {
		t.Errorf("layout %+v", l)
	}
	if out, err := exec.Command(ffmpeg, "-v", "error", "-xerror", "-i", l.Data.Path, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Errorf("remuxed clip doesn't decode: %v %s", err, out)
	}
}
