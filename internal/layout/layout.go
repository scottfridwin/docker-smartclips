// Package layout describes a virtual clip file as literal bytes and byte ranges of a data file.
package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// FormatVersion changes whenever layouts built by older code must be rebuilt.
const FormatVersion = 1

// Kinds of layout.
const (
	KindMKV   = "mkv"   // assembled from byte ranges of the original Matroska file
	KindRemux = "remux" // a whole file remuxed into the cache by ffmpeg
)

// Part is either literal bytes (Data) or a byte range of the data file (Off, Len).
type Part struct {
	Data []byte `json:"d,omitempty"`
	Off  int64  `json:"o,omitempty"`
	Len  int64  `json:"l"`
}

// FileID identifies a version of a file; a different size or mtime means it changed.
type FileID struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"` // unix nanoseconds
}

// Stat returns the current FileID of path.
func Stat(path string) (FileID, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FileID{}, err
	}
	if !info.Mode().IsRegular() {
		return FileID{}, fmt.Errorf("%s is not a regular file", path)
	}
	return FileID{Path: path, Size: info.Size(), ModTime: info.ModTime().UnixNano()}, nil
}

// Matches reports whether the open file f still is the version described by id.
func (id FileID) Matches(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Size() == id.Size && info.ModTime().UnixNano() == id.ModTime
}

// Layout is a clip ready to be served.
type Layout struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Origin  FileID `json:"origin"` // media file the clip is cut from
	Data    FileID `json:"data"`   // file the byte ranges refer to
	// Requested and actual clip bounds in seconds of the origin's timeline.
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	ActualStart float64 `json:"actual_start"`
	ActualEnd   float64 `json:"actual_end"`
	Size        int64   `json:"size"`
	Built       int64   `json:"built"` // unix nanoseconds; the clip's mtime
	Parts       []Part  `json:"parts"`

	offsets []int64 // start offset of each part
}

// Finalize validates the parts and computes the size and read index.
func (l *Layout) Finalize() error {
	l.offsets = make([]int64, len(l.Parts))
	var pos int64
	for i, p := range l.Parts {
		if p.Data != nil {
			if int64(len(p.Data)) != p.Len {
				return fmt.Errorf("layout: part %d length %d != %d", i, len(p.Data), p.Len)
			}
		} else if p.Len <= 0 || p.Off < 0 || p.Off+p.Len > l.Data.Size {
			return fmt.Errorf("layout: part %d range %d+%d outside data file of %d bytes", i, p.Off, p.Len, l.Data.Size)
		}
		l.offsets[i] = pos
		pos += p.Len
	}
	if l.Size != 0 && l.Size != pos {
		return fmt.Errorf("layout: size %d != parts total %d", l.Size, pos)
	}
	l.Size = pos
	return nil
}

// ReadAt fills p from offset off of the virtual file, reading ranges from data.
func (l *Layout) ReadAt(p []byte, off int64, data io.ReaderAt) (int, error) {
	if off >= l.Size {
		return 0, io.EOF
	}
	i := sort.Search(len(l.offsets), func(i int) bool { return l.offsets[i] > off }) - 1
	n := 0
	for ; i < len(l.Parts) && n < len(p); i++ {
		part := l.Parts[i]
		within := off + int64(n) - l.offsets[i]
		want := part.Len - within
		if rest := int64(len(p) - n); want > rest {
			want = rest
		}
		dst := p[n : n+int(want)]
		if part.Data != nil {
			copy(dst, part.Data[within:])
		} else {
			got, err := data.ReadAt(dst, part.Off+within)
			if got < len(dst) {
				if err == nil || errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				return n + got, err
			}
		}
		n += int(want)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Append adds a part, merging it with the previous one where contiguous.
func Append(parts []Part, p Part) []Part {
	if p.Len == 0 {
		return parts
	}
	if k := len(parts) - 1; k >= 0 {
		last := &parts[k]
		switch {
		case p.Data == nil && last.Data == nil && last.Off+last.Len == p.Off:
			last.Len += p.Len
			return parts
		case p.Data != nil && last.Data != nil:
			last.Data = append(last.Data, p.Data...)
			last.Len += p.Len
			return parts
		}
	}
	return append(parts, p)
}

// Literal is a Part holding b.
func Literal(b []byte) Part { return Part{Data: b, Len: int64(len(b))} }

// Range is a Part referring to n bytes of the data file at off.
func Range(off, n int64) Part { return Part{Off: off, Len: n} }

// Load reads a layout saved by Save.
func Load(path string) (*Layout, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Layout
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("layout %s: %w", path, err)
	}
	if l.Version != FormatVersion {
		return nil, fmt.Errorf("layout %s: format version %d, want %d", path, l.Version, FormatVersion)
	}
	if err := l.Finalize(); err != nil {
		return nil, fmt.Errorf("layout %s: %w", path, err)
	}
	return &l, nil
}

// Save writes the layout to path atomically.
func (l *Layout) Save(path string) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, b, 0o644)
}

// WriteFileAtomic writes data to a temporary file in the same directory, then renames it over path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
