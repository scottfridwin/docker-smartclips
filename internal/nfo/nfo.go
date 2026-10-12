// Package nfo renders clip metadata as a Kodi/Jellyfin NFO document.
package nfo

import (
	"encoding/xml"
	"sort"
	"strconv"
	"strings"
)

// Generate renders metadata under the root element nfoType ("movie" when empty).
// Keys are written in sorted order so the output is stable; invalid keys are skipped.
func Generate(nfoType string, metadata map[string]any, validKey func(string) bool) []byte {
	if nfoType == "" {
		nfoType = "movie"
	}
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<" + nfoType + ">\n")
	writeMap(&b, metadata, 1, validKey)
	b.WriteString("</" + nfoType + ">\n")
	return []byte(b.String())
}

func writeMap(b *strings.Builder, m map[string]any, indent int, validKey func(string) bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		if validKey(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeValue(b, k, m[k], indent, validKey)
	}
}

func writeValue(b *strings.Builder, key string, v any, indent int, validKey func(string) bool) {
	pad := strings.Repeat("  ", indent)
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			writeValue(b, key, item, indent, validKey)
		}
	case map[string]any:
		b.WriteString(pad + "<" + key + ">\n")
		writeMap(b, t, indent+1, validKey)
		b.WriteString(pad + "</" + key + ">\n")
	default:
		b.WriteString(pad + "<" + key + ">" + escape(scalar(t)) + "</" + key + ">\n")
	}
}

func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func escape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
