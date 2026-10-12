package ebml

import (
	"bytes"
	"testing"
)

func TestSizeRoundTrip(t *testing.T) {
	for _, n := range []int64{0, 1, 126, 127, 128, 16382, 16383, 16384, 1 << 30, 1<<56 - 2} {
		w := SizeWidth(n)
		b := AppendSizeWidth(nil, n, w)
		if len(b) != w {
			t.Fatalf("size %d: %d bytes, want %d", n, len(b), w)
		}
		got, l, err := ReadSize(b)
		if err != nil || got != n || l != w {
			t.Errorf("size %d: got %d (len %d, err %v)", n, got, l, err)
		}
	}
	// 127 in one byte is the reserved "unknown" value, so it must take two bytes.
	if SizeWidth(127) != 2 {
		t.Errorf("SizeWidth(127) = %d, want 2", SizeWidth(127))
	}
}

func TestUnknownSize(t *testing.T) {
	for _, b := range [][]byte{{0xFF}, {0x7F, 0xFF}, {0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}} {
		if n, _, err := ReadSize(b); err != nil || n != UnknownSize {
			t.Errorf("%x: got %d, %v", b, n, err)
		}
	}
}

func TestElementRoundTrip(t *testing.T) {
	var b []byte
	b = AppendUint(b, IDTimestamp, 0)
	b = AppendUint(b, IDTimestampScale, 1000000)
	b = AppendUintWidth(b, IDCueClusterPosition, 42, 8)
	b = AppendFloat(b, IDDuration, 1234.5)
	b = AppendElement(b, IDTitle, []byte("hello"))
	kids, err := Children(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 5 {
		t.Fatalf("%d children", len(kids))
	}
	if v, _ := Uint(kids[1].Data); v != 1000000 {
		t.Errorf("uint %d", v)
	}
	if len(kids[2].Data) != 8 {
		t.Errorf("fixed width %d", len(kids[2].Data))
	}
	if f, _ := Float(kids[3].Data); f != 1234.5 {
		t.Errorf("float %v", f)
	}
	if !bytes.Equal(kids[4].Data, []byte("hello")) || kids[4].ID != IDTitle {
		t.Errorf("title %x %q", kids[4].ID, kids[4].Data)
	}
}

func TestChildrenRejectsOverrun(t *testing.T) {
	b := AppendElement(nil, IDTitle, []byte("hello"))
	if _, err := Children(b[:len(b)-1]); err == nil {
		t.Error("truncated element accepted")
	}
	if _, err := Children([]byte{0x00}); err == nil {
		t.Error("invalid ID accepted")
	}
}
