package mkv

import (
	"bytes"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/scottfridwin/docker-smartclips/internal/ebml"
	"github.com/scottfridwin/docker-smartclips/internal/layout"
)

func openFixture(t *testing.T) ([]byte, *Source) {
	t.Helper()
	data, err := os.ReadFile("../../testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	src, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return data, src
}

func materialize(t *testing.T, data []byte, c *Clip) []byte {
	t.Helper()
	l := &layout.Layout{Data: layout.FileID{Size: int64(len(data))}, Parts: c.Parts}
	if err := l.Finalize(); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, l.Size)
	if n, err := l.ReadAt(out, 0, bytes.NewReader(data)); err != nil || int64(n) != l.Size {
		t.Fatalf("ReadAt: n=%d err=%v", n, err)
	}
	return out
}

func TestOpenFixture(t *testing.T) {
	_, src := openFixture(t)
	if src.VideoTrack == 0 {
		t.Fatal("no video track")
	}
	if src.TimestampScale != 1000000 {
		t.Errorf("timestamp scale %d", src.TimestampScale)
	}
	if d := src.DurationSeconds(); math.Abs(d-30) > 0.1 {
		t.Errorf("duration %.3f", d)
	}
	kf := src.Keyframes()
	if len(kf) != 15 {
		t.Fatalf("got %d keyframes, want 15", len(kf))
	}
	for i, k := range kf {
		if math.Abs(k-float64(2*i)) > 0.1 {
			t.Errorf("keyframe %d at %.3f, want ~%d", i, k, 2*i)
		}
	}
}

// clipInfo is what a clip file contains, read back with the package's own parser.
type clipInfo struct {
	src      *Source
	clusters []*cluster
	blocks   []block
}

func readBack(t *testing.T, clip []byte) clipInfo {
	t.Helper()
	r := bytes.NewReader(clip)
	src, err := Open(r, int64(len(clip)))
	if err != nil {
		t.Fatalf("clip doesn't parse: %v", err)
	}
	var info clipInfo
	info.src = src
	for off := src.FirstCluster; off < src.SegmentEnd; {
		c, err := readCluster(r, off)
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			h, err := ebml.ReadHeaderAt(r, off)
			if err != nil {
				t.Fatal(err)
			}
			t.Fatalf("unexpected element 0x%X after the clusters", h.ID)
		}
		blocks, err := readBlocks(r, c)
		if err != nil {
			t.Fatal(err)
		}
		info.clusters = append(info.clusters, c)
		info.blocks = append(info.blocks, blocks...)
		off = c.end
	}
	if src.SegmentEnd != int64(len(clip)) {
		t.Errorf("segment ends at %d, file is %d bytes", src.SegmentEnd, len(clip))
	}
	return info
}

func TestBuildClip(t *testing.T) {
	data, src := openFixture(t)
	tests := []struct {
		name       string
		start, end float64
		wantStart  float64 // keyframe the clip starts at
		wantMaxEnd float64
	}{
		{"on keyframe", 4, 10, 4, 10},
		{"mid GOP", 5.3, 9.1, 4, 9.1},
		{"from the beginning", 0, 3, 0, 3},
		{"inside one cluster", 6.2, 7.0, 6, 7},
		{"past the end", 26, 99, 26, 30.1},
		{"whole file", 0, 30.1, 0, 30.1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := src.BuildClip(bytes.NewReader(data), tc.start, tc.end, "Test clip")
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(c.ActualStart-tc.wantStart) > 0.05 {
				t.Errorf("actual start %.3f, want %.3f", c.ActualStart, tc.wantStart)
			}
			clip := materialize(t, data, c)
			got := readBack(t, clip)

			if len(got.clusters) == 0 {
				t.Fatal("no clusters")
			}
			var prev uint64
			for i, cl := range got.clusters {
				if i > 0 && cl.timestamp < prev {
					t.Errorf("cluster %d timestamp %d < previous %d", i, cl.timestamp, prev)
				}
				prev = cl.timestamp
			}
			if got.clusters[0].timestamp != 0 {
				t.Errorf("first cluster at %d, want 0", got.clusters[0].timestamp)
			}

			// Video: starts with the keyframe and matches the source blocks byte for byte.
			var video []block
			for _, b := range got.blocks {
				if b.track == got.src.VideoTrack {
					video = append(video, b)
				}
			}
			if len(video) == 0 {
				t.Fatal("no video blocks")
			}
			first := clip[video[0].off : video[0].off+video[0].len]
			if !isKeyframe(t, first) {
				t.Error("clip doesn't start with a keyframe")
			}
			for _, b := range got.blocks {
				shifted := c.ActualStart + float64(b.time)/1000
				if b.track == got.src.VideoTrack && shifted >= tc.wantMaxEnd+0.001 {
					t.Errorf("video block at %.3f is past the end %.3f", shifted, tc.wantMaxEnd)
				}
				payload := clip[b.off : b.off+b.len]
				if !bytes.Contains(data, payload[b.tsOff+2:]) {
					t.Errorf("block at %d isn't a byte-for-byte copy of a source block", b.off)
				}
			}

			// Cues point at clusters in the clip.
			if len(got.src.Cues) == 0 {
				t.Fatal("clip has no cues")
			}
			starts := map[int64]bool{}
			for _, cl := range got.clusters {
				starts[cl.off] = true
			}
			for _, cp := range got.src.Cues {
				if !starts[cp.ClusterOff] {
					t.Errorf("cue at %d points at %d, which isn't a cluster", cp.Time, cp.ClusterOff)
				}
			}
			wantDur := math.Min(tc.end, src.DurationSeconds()) - tc.wantStart
			if d := got.src.DurationSeconds(); math.Abs(d-wantDur) > 0.1 {
				t.Errorf("duration %.3f, want %.3f", d, wantDur)
			}
			title := ""
			for _, el := range got.src.InfoChildren {
				if el.ID == ebml.IDTitle {
					title = string(el.Data)
				}
			}
			if title != "Test clip" {
				t.Errorf("title %q", title)
			}

			// Almost all of the clip must be referenced from the source, not copied: the
			// header, cluster headers and the first cluster's block headers.
			var literal int64
			for _, p := range c.Parts {
				if p.Data != nil {
					literal += p.Len
				}
			}
			if limit := int64(4096 + 64*len(got.clusters) + 16*len(got.blocks)); literal > limit {
				t.Errorf("%d literal bytes, want <= %d", literal, limit)
			}
		})
	}
}

func isKeyframe(t *testing.T, raw []byte) bool {
	t.Helper()
	h, err := ebml.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != ebml.IDSimpleBlock {
		return true // a BlockGroup without a ReferenceBlock is a keyframe; ffmpeg uses SimpleBlocks for video
	}
	_, tl, err := ebml.ReadSize(raw[h.HeaderLen:])
	if err != nil {
		t.Fatal(err)
	}
	return raw[h.HeaderLen+tl+2]&0x80 != 0
}

func TestBuildClipErrors(t *testing.T) {
	data, src := openFixture(t)
	r := bytes.NewReader(data)
	if _, err := src.BuildClip(r, 10, 10, "x"); err == nil {
		t.Error("empty range accepted")
	}
	if _, err := src.BuildClip(r, 31, 40, "x"); err == nil {
		t.Error("start past the end accepted")
	}
}

func TestOpenRejects(t *testing.T) {
	data, _ := openFixture(t)
	cases := map[string][]byte{
		"empty":       {},
		"not ebml":    []byte("this is not a matroska file at all"),
		"truncated":   data[:100],
		"no clusters": data[:1500],
	}
	for name, b := range cases {
		if _, err := Open(bytes.NewReader(b), int64(len(b))); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Open(bytes.NewReader(cases["not ebml"]), 34); !errors.Is(err, ErrUnsupported) {
		t.Errorf("non-EBML input should be ErrUnsupported, got %v", err)
	}
}

// TestDecodesLikeSource checks that ffmpeg decodes the clip without errors and that its
// frames equal the source's. It is skipped when ffmpeg isn't installed.
func TestDecodesLikeSource(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	data, src := openFixture(t)
	c, err := src.BuildClip(bytes.NewReader(data), 9.5, 21, "Decode test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "clip.mkv")
	if err := os.WriteFile(path, materialize(t, data, c), 0o600); err != nil {
		t.Fatal(err)
	}
	// ffmpeg's null muxer warns about repeated DTS even for the untouched source (millisecond
	// timestamps with B-frames), so only the exit status counts.
	if out, err := exec.Command(ffmpeg, "-v", "error", "-xerror", "-i", path, "-map", "0:v", "-map", "0:a", "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	hash := func(file string, ss float64) string {
		out, err := exec.Command(ffmpeg, "-v", "error", "-ss", strconv.FormatFloat(ss, 'f', 3, 64), "-i", file, "-map", "0:v",
			"-frames:v", "24", "-f", "framemd5", "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		var sums [][]byte
		for _, line := range bytes.Split(out, []byte("\n")) {
			if f := bytes.Split(line, []byte(",")); len(line) > 0 && line[0] != '#' && len(f) == 6 {
				sums = append(sums, bytes.TrimSpace(f[5]))
			}
		}
		return string(bytes.Join(sums, nil))
	}
	for _, off := range []float64{0.5, 5} {
		if hash(path, off) != hash("../../testdata/sample.mkv", c.ActualStart+off) {
			t.Errorf("frames at +%.1fs differ from the source", off)
		}
	}
}
