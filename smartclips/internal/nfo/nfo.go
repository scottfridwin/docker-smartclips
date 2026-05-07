package nfo

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// Generate produces an NFO XML document from a metadata map.
// nfoType determines the root element (e.g., "movie", "episodedetails", "musicvideo").
// If nfoType is empty, defaults to "movie".
func Generate(nfoType string, metadata map[string]interface{}) []byte {
	if nfoType == "" {
		nfoType = "movie"
	}

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(fmt.Sprintf("<%s>\n", nfoType))

	for key, value := range metadata {
		writeElement(&b, key, value, 1)
	}

	b.WriteString(fmt.Sprintf("</%s>\n", nfoType))
	return []byte(b.String())
}

func writeElement(b *strings.Builder, key string, value interface{}, indent int) {
	prefix := strings.Repeat("  ", indent)

	switch v := value.(type) {
	case string:
		b.WriteString(fmt.Sprintf("%s<%s>%s</%s>\n", prefix, key, xmlEscape(v), key))
	case float64:
		// JSON numbers decode as float64
		if v == float64(int64(v)) {
			b.WriteString(fmt.Sprintf("%s<%s>%d</%s>\n", prefix, key, int64(v), key))
		} else {
			b.WriteString(fmt.Sprintf("%s<%s>%g</%s>\n", prefix, key, v, key))
		}
	case bool:
		b.WriteString(fmt.Sprintf("%s<%s>%t</%s>\n", prefix, key, v, key))
	case []interface{}:
		// Array: each element becomes a repeated element or nested object
		for _, item := range v {
			switch obj := item.(type) {
			case map[string]interface{}:
				// Nested object: <key><child>val</child>...</key>
				b.WriteString(fmt.Sprintf("%s<%s>\n", prefix, key))
				for childKey, childVal := range obj {
					writeElement(b, childKey, childVal, indent+1)
				}
				b.WriteString(fmt.Sprintf("%s</%s>\n", prefix, key))
			default:
				// Repeated simple element: <key>value</key>
				writeElement(b, key, obj, indent)
			}
		}
	case map[string]interface{}:
		// Single nested object
		b.WriteString(fmt.Sprintf("%s<%s>\n", prefix, key))
		for childKey, childVal := range v {
			writeElement(b, childKey, childVal, indent+1)
		}
		b.WriteString(fmt.Sprintf("%s</%s>\n", prefix, key))
	default:
		b.WriteString(fmt.Sprintf("%s<%s>%v</%s>\n", prefix, key, value, key))
	}
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
