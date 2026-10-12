package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mediaRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Movies", "A"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Movies", "A", "a.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.mkv")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.mkv")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestResolveInput(t *testing.T) {
	root := mediaRoot(t)
	ok := []string{"Movies/A/a.mkv", filepath.Join(root, "Movies/A/a.mkv"), "Movies/A/../A/a.mkv"}
	for _, in := range ok {
		if _, err := ResolveInput(root, in); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
	bad := []string{"", "../outside.mkv", "/etc/passwd", "Movies/A", "missing.mkv", "escape.mkv", "a\x00b"}
	for _, in := range bad {
		if _, err := ResolveInput(root, in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestValidate(t *testing.T) {
	root := mediaRoot(t)
	good := Clip{Input: "Movies/A/a.mkv", Output: "Clip", Start: 1, End: 2}
	cases := map[string]Clip{
		"slash in name":     {Input: good.Input, Output: "a/b", Start: 1, End: 2},
		"dot dot name":      {Input: good.Input, Output: "..", Start: 1, End: 2},
		"hidden name":       {Input: good.Input, Output: ".x", Start: 1, End: 2},
		"traversal group":   {Input: good.Input, Output: "x", Group: "a/../b", Start: 1, End: 2},
		"control character": {Input: good.Input, Output: "a\nb", Start: 1, End: 2},
		"end before start":  {Input: good.Input, Output: "x", Start: 5, End: 2},
		"negative start":    {Input: good.Input, Output: "x", Start: -1, End: 2},
		"too long":          {Input: good.Input, Output: "x", Start: 0, End: MaxClipSeconds + 1},
		"input outside":     {Input: "/etc/hostname", Output: "x", Start: 1, End: 2},
		"unknown nfo type":  {Input: good.Input, Output: "x", Start: 1, End: 2, NfoType: "script"},
		"bad metadata key":  {Input: good.Input, Output: "x", Start: 1, End: 2, Metadata: map[string]any{"a b": "c"}},
		"xml-prefixed key":  {Input: good.Input, Output: "x", Start: 1, End: 2, Metadata: map[string]any{"xmlns": "c"}},
		"nested bad key":    {Input: good.Input, Output: "x", Start: 1, End: 2, Metadata: map[string]any{"actor": []any{map[string]any{"<x>": 1.0}}}},
		"too deeply nested": {Input: good.Input, Output: "x", Start: 1, End: 2, Metadata: map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": map[string]any{}}}}}},
	}
	if err := Validate([]Clip{good}, root); err != nil {
		t.Fatalf("valid clip rejected: %v", err)
	}
	for name, c := range cases {
		err := Validate([]Clip{c}, root)
		var ve *ValidationError
		if !errors.As(err, &ve) || len(ve.Problems) == 0 || ve.Problems[0].Index != 0 {
			t.Errorf("%s: got %v", name, err)
		}
	}
	dup := []Clip{good, {Input: good.Input, Output: "CLIP", Start: 1, End: 2}}
	if err := Validate(dup, root); err == nil || !strings.Contains(err.Error(), "same path") {
		t.Errorf("case-insensitive duplicate accepted: %v", err)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte(`[{"input":"a","output":"b","start":0,"end":1,"typo":true}]`)); err == nil {
		t.Error("unknown field accepted")
	}
	if clips, err := Parse([]byte("  \n")); err != nil || clips != nil {
		t.Errorf("empty file: %v %v", clips, err)
	}
}

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clips.json")
	f, err := Load(path)
	if err != nil || len(f.Clips) != 0 {
		t.Fatalf("missing file: %+v %v", f, err)
	}
	clips := []Clip{{Input: "a.mkv", Output: "A", Start: 1, End: 2, Metadata: map[string]any{"year": 2002.0}}}
	v, err := Save(path, clips)
	if err != nil {
		t.Fatal(err)
	}
	f, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Version != v || len(f.Clips) != 1 || f.Clips[0].Output != "A" {
		t.Errorf("loaded %+v (version %s, want %s)", f, f.Version, v)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o664 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
}
