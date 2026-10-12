package nfo

import (
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	valid := func(k string) bool { return k != "bad key" }
	got := string(Generate("musicvideo", map[string]any{
		"title":   "Cell Block <Tango> & co",
		"year":    float64(2002),
		"rating":  7.5,
		"genre":   []any{"Musical", "Drama"},
		"actor":   []any{map[string]any{"name": "X", "role": "Y"}},
		"bad key": "ignored",
		"watched": false,
	}, valid))
	want := `<?xml version="1.0" encoding="UTF-8"?>
<musicvideo>
  <actor>
    <name>X</name>
    <role>Y</role>
  </actor>
  <genre>Musical</genre>
  <genre>Drama</genre>
  <rating>7.5</rating>
  <title>Cell Block &lt;Tango&gt; &amp; co</title>
  <watched>false</watched>
  <year>2002</year>
</musicvideo>
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestGenerateDefaultRoot(t *testing.T) {
	got := string(Generate("", nil, func(string) bool { return true }))
	if !strings.Contains(got, "<movie>\n</movie>") {
		t.Errorf("got %s", got)
	}
}
