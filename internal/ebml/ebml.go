// Package ebml reads and writes the EBML element framing used by Matroska.
package ebml

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Matroska element IDs used by smartclips.
const (
	IDEBML               = 0x1A45DFA3
	IDSegment            = 0x18538067
	IDSeekHead           = 0x114D9B74
	IDSeek               = 0x4DBB
	IDSeekID             = 0x53AB
	IDSeekPosition       = 0x53AC
	IDInfo               = 0x1549A966
	IDTimestampScale     = 0x2AD7B1
	IDDuration           = 0x4489
	IDTitle              = 0x7BA9
	IDTracks             = 0x1654AE6B
	IDTrackEntry         = 0xAE
	IDTrackNumber        = 0xD7
	IDTrackType          = 0x83
	IDCues               = 0x1C53BB6B
	IDCuePoint           = 0xBB
	IDCueTime            = 0xB3
	IDCueTrackPositions  = 0xB7
	IDCueTrack           = 0xF7
	IDCueClusterPosition = 0xF1
	IDCluster            = 0x1F43B675
	IDTimestamp          = 0xE7
	IDSimpleBlock        = 0xA3
	IDBlockGroup         = 0xA0
	IDBlock              = 0xA1
	IDEncryptedBlock     = 0xAF
	IDCRC32              = 0xBF
	IDVoid               = 0xEC
	IDAttachments        = 0x1941A469
	IDChapters           = 0x1043A770
	IDTags               = 0x1254C367
)

// UnknownSize is the Size of an element whose length is not encoded ("live" streams).
const UnknownSize = -1

// ErrShort means the buffer ended inside an element header.
var ErrShort = errors.New("ebml: short buffer")

// Header is a parsed element header.
type Header struct {
	ID        uint32
	Size      int64 // payload size, or UnknownSize
	HeaderLen int   // bytes used by the ID and size fields
}

func vintLen(b0 byte) int {
	for n := 1; n <= 8; n++ {
		if b0&(0x80>>(n-1)) != 0 {
			return n
		}
	}
	return 0
}

// ReadID decodes an element ID (marker bit kept), returning it and its length.
func ReadID(b []byte) (uint32, int, error) {
	if len(b) == 0 {
		return 0, 0, ErrShort
	}
	n := vintLen(b[0])
	if n == 0 || n > 4 {
		return 0, 0, fmt.Errorf("ebml: invalid element ID byte 0x%02x", b[0])
	}
	if len(b) < n {
		return 0, 0, ErrShort
	}
	var v uint32
	for i := 0; i < n; i++ {
		v = v<<8 | uint32(b[i])
	}
	return v, n, nil
}

// ReadSize decodes an element size, returning UnknownSize for the reserved all-ones value.
func ReadSize(b []byte) (int64, int, error) {
	if len(b) == 0 {
		return 0, 0, ErrShort
	}
	n := vintLen(b[0])
	if n == 0 {
		return 0, 0, fmt.Errorf("ebml: invalid size byte 0x%02x", b[0])
	}
	if len(b) < n {
		return 0, 0, ErrShort
	}
	v := uint64(b[0]) & (0xFF >> n)
	allOnes := v == 0xFF>>n
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
		allOnes = allOnes && b[i] == 0xFF
	}
	if allOnes {
		return UnknownSize, n, nil
	}
	if v > math.MaxInt64 {
		return 0, 0, fmt.Errorf("ebml: size %d overflows", v)
	}
	return int64(v), n, nil
}

// ParseHeader decodes the element header at the start of b.
func ParseHeader(b []byte) (Header, error) {
	id, il, err := ReadID(b)
	if err != nil {
		return Header{}, err
	}
	size, sl, err := ReadSize(b[il:])
	if err != nil {
		return Header{}, err
	}
	return Header{ID: id, Size: size, HeaderLen: il + sl}, nil
}

// ReadHeaderAt reads and decodes the element header at off.
func ReadHeaderAt(r io.ReaderAt, off int64) (Header, error) {
	var buf [12]byte
	n, err := r.ReadAt(buf[:], off)
	if n == 0 && err != nil {
		return Header{}, err
	}
	return ParseHeader(buf[:n])
}

// Element is a child element inside a buffered master element.
type Element struct {
	ID   uint32
	Data []byte // payload
	Raw  []byte // header + payload
}

// Children decodes the elements in b. Unknown sizes are rejected.
func Children(b []byte) ([]Element, error) {
	var out []Element
	for p := 0; p < len(b); {
		h, err := ParseHeader(b[p:])
		if err != nil {
			return nil, err
		}
		if h.Size == UnknownSize {
			return nil, fmt.Errorf("ebml: unknown-size element 0x%X inside a sized parent", h.ID)
		}
		end := p + h.HeaderLen + int(h.Size)
		if h.Size > int64(len(b)) || end > len(b) {
			return nil, fmt.Errorf("ebml: element 0x%X overruns its parent", h.ID)
		}
		out = append(out, Element{ID: h.ID, Data: b[p+h.HeaderLen : end], Raw: b[p:end]})
		p = end
	}
	return out, nil
}

// Uint decodes an unsigned integer payload.
func Uint(b []byte) (uint64, error) {
	if len(b) > 8 {
		return 0, fmt.Errorf("ebml: %d-byte unsigned integer", len(b))
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

// Float decodes a 4- or 8-byte float payload.
func Float(b []byte) (float64, error) {
	switch len(b) {
	case 0:
		return 0, nil
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	}
	return 0, fmt.Errorf("ebml: %d-byte float", len(b))
}

// AppendID appends an element ID.
func AppendID(dst []byte, id uint32) []byte {
	switch {
	case id > 0xFFFFFF:
		return append(dst, byte(id>>24), byte(id>>16), byte(id>>8), byte(id))
	case id > 0xFFFF:
		return append(dst, byte(id>>16), byte(id>>8), byte(id))
	case id > 0xFF:
		return append(dst, byte(id>>8), byte(id))
	}
	return append(dst, byte(id))
}

// AppendSizeWidth appends size n encoded in exactly width bytes (1-8).
func AppendSizeWidth(dst []byte, n int64, width int) []byte {
	v := uint64(n) | 1<<(7*uint(width))
	for i := width - 1; i >= 0; i-- {
		dst = append(dst, byte(v>>(8*uint(i))))
	}
	return dst
}

// SizeWidth is the minimal width that encodes n without hitting the reserved value.
func SizeWidth(n int64) int {
	w := 1
	for w < 8 && uint64(n) >= 1<<(7*uint(w))-1 {
		w++
	}
	return w
}

// AppendElement appends an element with a minimally encoded size.
func AppendElement(dst []byte, id uint32, payload []byte) []byte {
	dst = AppendID(dst, id)
	dst = AppendSizeWidth(dst, int64(len(payload)), SizeWidth(int64(len(payload))))
	return append(dst, payload...)
}

// AppendUint appends an unsigned integer element using the fewest bytes.
func AppendUint(dst []byte, id uint32, v uint64) []byte {
	n := 1
	for n < 8 && v>>(8*uint(n)) != 0 {
		n++
	}
	return AppendUintWidth(dst, id, v, n)
}

// AppendUintWidth appends an unsigned integer element in exactly width bytes.
func AppendUintWidth(dst []byte, id uint32, v uint64, width int) []byte {
	buf := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		buf[i] = byte(v)
		v >>= 8
	}
	return AppendElement(dst, id, buf)
}

// AppendFloat appends an 8-byte float element.
func AppendFloat(dst []byte, id uint32, f float64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], math.Float64bits(f))
	return AppendElement(dst, id, buf[:])
}
