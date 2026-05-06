package config

import (
	"encoding/json"
	"os"
)

type Clip struct {
	Input  string  `json:"input"`
	Output string  `json:"output"`
	Start  float64 `json:"start"`
	End    float64 `json:"end"`
}

func Load(path string) ([]Clip, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var clips []Clip
	if err := json.Unmarshal(data, &clips); err != nil {
		return nil, err
	}

	return clips, nil
}
