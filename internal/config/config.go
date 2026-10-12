// Package config loads, validates and saves the clip definitions file.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/scottfridwin/docker-smartclips/internal/layout"
)

// MaxClipSeconds bounds a single clip's length.
const MaxClipSeconds = 6 * 3600

// Clip is one clip definition.
type Clip struct {
	Input    string         `json:"input"`
	Output   string         `json:"output"`
	Start    float64        `json:"start"`
	End      float64        `json:"end"`
	Group    string         `json:"group,omitempty"`
	NfoType  string         `json:"nfo_type,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Path is the clip's slash-separated path below the mount, without extension.
func (c Clip) Path() string {
	g := strings.Trim(c.Group, "/")
	if g == "" {
		return c.Output
	}
	return g + "/" + c.Output
}

// NfoTypes are the allowed NFO root elements.
var NfoTypes = []string{"movie", "episodedetails", "musicvideo"}

var xmlName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)

// ValidKey reports whether k can be used as an NFO element name.
func ValidKey(k string) bool {
	return xmlName.MatchString(k) && !strings.HasPrefix(strings.ToLower(k), "xml")
}

// File is a loaded clips file.
type File struct {
	Clips   []Clip
	ModTime time.Time
	Version string // content hash, for optimistic concurrency
}

// Version returns the content hash used to detect concurrent edits.
func Version(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// Load reads path. A missing file is an empty clip list.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &File{Version: Version(nil)}, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	clips, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &File{Clips: clips, ModTime: info.ModTime(), Version: Version(data)}, nil
}

// Parse decodes a clips file.
func Parse(data []byte) ([]Clip, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var clips []Clip
	if err := dec.Decode(&clips); err != nil {
		return nil, err
	}
	return clips, nil
}

// Encode formats clips the way Save writes them.
func Encode(clips []Clip) ([]byte, error) {
	if clips == nil {
		clips = []Clip{}
	}
	b, err := json.MarshalIndent(clips, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Save writes clips to path atomically and returns the new version.
func Save(path string, clips []Clip) (string, error) {
	b, err := Encode(clips)
	if err != nil {
		return "", err
	}
	if err := layout.WriteFileAtomic(path, b, 0o664); err != nil {
		return "", err
	}
	return Version(b), nil
}

// ValidationError lists every problem found, keyed by clip index.
type ValidationError struct {
	Problems []Problem `json:"problems"`
}

// Problem is one validation failure.
type Problem struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		msgs[i] = fmt.Sprintf("clip %d: %s", p.Index+1, p.Message)
	}
	return strings.Join(msgs, "; ")
}

// Validate checks every clip; mediaRoot confines input paths. It returns *ValidationError.
func Validate(clips []Clip, mediaRoot string) error {
	var probs []Problem
	seen := make(map[string]int, len(clips))
	for i, c := range clips {
		add := func(format string, args ...any) {
			probs = append(probs, Problem{Index: i, Message: fmt.Sprintf(format, args...)})
		}
		if err := checkName(c.Output); err != nil {
			add("output %v", err)
		}
		if g := strings.Trim(c.Group, "/"); g != "" {
			for _, part := range strings.Split(g, "/") {
				if err := checkName(part); err != nil {
					add("group folder %q %v", part, err)
				}
			}
		}
		if j, dup := seen[strings.ToLower(c.Path())]; dup {
			add("same path as clip %d (%q)", j+1, c.Path())
		} else {
			seen[strings.ToLower(c.Path())] = i
		}
		if c.Start < 0 || c.End <= c.Start {
			add("end (%.3f) must be after start (%.3f), and start must not be negative", c.End, c.Start)
		} else if c.End-c.Start > MaxClipSeconds {
			add("clips can be at most %d hours long", MaxClipSeconds/3600)
		}
		if _, err := ResolveInput(mediaRoot, c.Input); err != nil {
			add("input %v", err)
		}
		if c.NfoType != "" && !contains(NfoTypes, c.NfoType) {
			add("nfo_type must be one of %s", strings.Join(NfoTypes, ", "))
		}
		if err := checkMetadata(c.Metadata, 0); err != nil {
			add("metadata %v", err)
		}
	}
	if len(probs) > 0 {
		return &ValidationError{Problems: probs}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func checkName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("is empty")
	case name == "." || name == "..":
		return errors.New("is not a valid name")
	case strings.ContainsAny(name, "/\\\x00"):
		return errors.New("must not contain '/', '\\' or NUL")
	case strings.HasPrefix(name, "."):
		return errors.New("must not start with '.'")
	case len(name) > 200:
		return errors.New("is longer than 200 bytes")
	case name != strings.TrimSpace(name):
		return errors.New("must not start or end with spaces")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

func checkMetadata(v any, depth int) error {
	if depth > 3 {
		return errors.New("is nested too deeply")
	}
	switch t := v.(type) {
	case nil, string, float64, bool:
		return nil
	case map[string]any:
		for k, child := range t {
			if !ValidKey(k) {
				return fmt.Errorf("key %q is not a valid XML element name", k)
			}
			if err := checkMetadata(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, child := range t {
			if err := checkMetadata(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("has an unsupported value of type %T", v)
}

// ResolveInput maps a clip input to an absolute path inside mediaRoot.
// Relative inputs are relative to mediaRoot; absolute inputs must be inside it.
// Symlinks are resolved so they can't point outside the root.
func ResolveInput(mediaRoot, input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		return "", errors.New("is empty")
	}
	if strings.ContainsRune(input, 0) {
		return "", errors.New("contains NUL")
	}
	root, err := filepath.Abs(mediaRoot)
	if err != nil {
		return "", err
	}
	p := input
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	if !within(root, p) {
		return "", fmt.Errorf("%q is outside the media folder", input)
	}
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		if alt, ok := Replacement(p); ok {
			p = alt
		}
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%q does not exist", input)
		}
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if !within(realRoot, resolved) {
		return "", fmt.Errorf("%q resolves outside the media folder", input)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a file", input)
	}
	return p, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// VideoExts are the file extensions treated as video.
var VideoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".ts": true, ".mov": true, ".webm": true, ".mk3d": true,
}

var (
	// "<title> - [edition]...[quality]<anything>.<ext>", as written by Radarr and Sonarr.
	taggedName = regexp.MustCompile(`^(.*? - )((?:\[[^\]]*\])+)(.*)\.[A-Za-z0-9]+$`)
	bracketTag = regexp.MustCompile(`\[[^\]]*\]`)
	qualityTag = regexp.MustCompile(`(?i)\d{3,4}p|\b(blu-?ray|web(-?dl|rip)?|hdtv|dvd(rip|scr)?|sdtv|remux|raw-hd|br-disk|unknown|telesync|telecine|workprint|cam)\b`)
)

// identity is a file name without its quality tag, what follows it and the extension, so
// "Movie (1999) - [Theatrical][Bluray-1080p].mkv" and its 2160p upgrade share one identity.
// Tags before the quality tag (editions) are kept, so different cuts stay distinct.
func identity(name string) (string, bool) {
	m := taggedName.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	tags := bracketTag.FindAllString(m[2], -1)
	for i, t := range tags {
		if qualityTag.MatchString(t) {
			return m[1] + strings.Join(tags[:i], ""), true
		}
	}
	return "", false
}

// Replacement finds the file that replaced the missing file p after a quality upgrade: a
// video in the same folder with the same title and edition tags. The newest one wins.
func Replacement(p string) (string, bool) {
	want, ok := identity(filepath.Base(p))
	if !ok {
		return "", false
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		return "", false
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		name := e.Name()
		if !VideoExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		if id, ok := identity(name); !ok || id != want {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = filepath.Join(filepath.Dir(p), name), info.ModTime()
		}
	}
	return best, best != ""
}

// Relative returns p relative to mediaRoot when it is inside it, otherwise p.
func Relative(mediaRoot, p string) string {
	if rel, err := filepath.Rel(mediaRoot, p); err == nil && within(mediaRoot, p) {
		return filepath.ToSlash(rel)
	}
	return p
}
