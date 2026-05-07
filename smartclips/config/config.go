package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Clip struct {
	Input    string                 `json:"input"`
	Output   string                 `json:"output"`
	Start    float64                `json:"start"`
	End      float64                `json:"end"`
	Group    string                 `json:"group,omitempty"`
	NfoType  string                 `json:"nfo_type,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// FullPath returns the slash-separated directory path + filename for this clip.
// e.g. Group="Show/Season 01", Output="Cold Open" → "Show/Season 01/Cold Open"
func (c *Clip) FullPath() string {
	if c.Group == "" {
		return c.Output
	}
	return c.Group + "/" + c.Output
}

func Load(path string, mediaPrefix string) ([]Clip, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var clips []Clip
	if err := json.Unmarshal(data, &clips); err != nil {
		return nil, err
	}

	// Validate unique output paths (group + output combo)
	seen := make(map[string]bool, len(clips))
	for _, c := range clips {
		key := c.FullPath()
		if seen[key] {
			return nil, fmt.Errorf("duplicate clip path: %q", key)
		}
		seen[key] = true
	}

	// If a media prefix is set, prepend it to relative input paths
	if mediaPrefix != "" {
		for i := range clips {
			if !strings.HasPrefix(clips[i].Input, "/") {
				clips[i].Input = filepath.Join(mediaPrefix, clips[i].Input)
			}
		}
	}

	return clips, nil
}
