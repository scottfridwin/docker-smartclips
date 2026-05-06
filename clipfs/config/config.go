package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Clip struct {
	Input  string  `json:"input"`
	Output string  `json:"output"`
	Start  float64 `json:"start"`
	End    float64 `json:"end"`
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

	// Validate unique output names
	seen := make(map[string]bool, len(clips))
	for _, c := range clips {
		if seen[c.Output] {
			return nil, fmt.Errorf("duplicate output name: %q", c.Output)
		}
		seen[c.Output] = true
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
