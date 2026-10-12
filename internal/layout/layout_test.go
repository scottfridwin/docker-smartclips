package layout

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"
)

func TestReadAt(t *testing.T) {
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	l := &Layout{
		Version: FormatVersion,
		Data:    FileID{Size: int64(len(data))},
		Parts: []Part{
			Literal([]byte("<head>")),
			Range(10, 6), // abcdef
			Literal([]byte("|")),
			Range(0, 4), // 0123
		},
	}
	if err := l.Finalize(); err != nil {
		t.Fatal(err)
	}
	want := []byte("<head>abcdef|0123")
	if l.Size != int64(len(want)) {
		t.Fatalf("size %d", l.Size)
	}
	src := bytes.NewReader(data)
	for off := 0; off < len(want); off++ {
		for n := 1; off+n <= len(want)+2; n++ {
			buf := make([]byte, n)
			got, err := l.ReadAt(buf, int64(off), src)
			end := min(off+n, len(want))
			if !bytes.Equal(buf[:got], want[off:end]) {
				t.Fatalf("ReadAt(%d, %d) = %q, want %q", off, n, buf[:got], want[off:end])
			}
			if off+n > len(want) && err != io.EOF {
				t.Fatalf("ReadAt(%d, %d) past the end: err %v", off, n, err)
			}
		}
	}
}

func TestFinalizeRejectsBadRanges(t *testing.T) {
	l := &Layout{Data: FileID{Size: 10}, Parts: []Part{Range(5, 6)}}
	if err := l.Finalize(); err == nil {
		t.Error("range past the end of the data file accepted")
	}
	l = &Layout{Data: FileID{Size: 10}, Parts: []Part{{Data: []byte("ab"), Len: 3}}}
	if err := l.Finalize(); err == nil {
		t.Error("literal with wrong length accepted")
	}
}

func TestAppendMerges(t *testing.T) {
	var p []Part
	p = Append(p, Range(0, 10))
	p = Append(p, Range(10, 5))
	p = Append(p, Literal([]byte("a")))
	p = Append(p, Literal([]byte("b")))
	p = Append(p, Range(20, 5))
	if len(p) != 3 || p[0].Len != 15 || string(p[1].Data) != "ab" || p[2].Off != 20 {
		t.Errorf("merged parts: %+v", p)
	}
}

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.json")
	l := &Layout{Version: FormatVersion, Kind: KindMKV, Data: FileID{Path: "/x", Size: 100},
		Parts: []Part{Literal([]byte{0, 1, 2}), Range(50, 50)}}
	if err := l.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 53 || !bytes.Equal(got.Parts[0].Data, []byte{0, 1, 2}) || got.Data != l.Data {
		t.Errorf("loaded %+v", got)
	}
}
