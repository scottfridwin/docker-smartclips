// Package backup keeps timestamped copies of the clips file in a backup directory.
package backup

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/scottfridwin/docker-smartclips/internal/layout"
)

const (
	prefix = "clips-"
	suffix = ".json"
	stamp  = "20060102T150405Z"
)

// Backup copies Src into Dir whenever its content changed since the newest copy.
type Backup struct {
	Src      string
	Dir      string
	Keep     int
	Interval time.Duration
	Log      *slog.Logger

	mu      sync.Mutex
	lastRun time.Time
	lastErr error
}

// Status reports the newest copy and the outcome of the last run.
type Status struct {
	Dir     string    `json:"dir"`
	Latest  time.Time `json:"latest,omitempty"`
	Copies  int       `json:"copies"`
	LastRun time.Time `json:"last_run,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// Run backs up immediately and then every Interval until stop is closed.
func (b *Backup) Run(stop <-chan struct{}) {
	t := time.NewTicker(b.Interval)
	defer t.Stop()
	for {
		b.record(b.Once(time.Now()))
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

func (b *Backup) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastRun, b.lastErr = time.Now(), err
	if err != nil {
		b.Log.Error("backup failed", "dir", b.Dir, "err", err)
	}
}

// copies lists the backup files, newest first.
func (b *Backup) copies() ([]string, error) {
	entries, err := os.ReadDir(b.Dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
			if _, err := time.Parse(stamp, strings.TrimSuffix(strings.TrimPrefix(n, prefix), suffix)); err == nil {
				names = append(names, n)
			}
		}
	}
	// The timestamp format sorts chronologically.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// Once writes a copy if the clips file differs from the newest copy, then prunes old copies.
func (b *Backup) Once(now time.Time) error {
	data, err := os.ReadFile(b.Src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing to back up yet
	}
	if err != nil {
		return err
	}
	names, err := b.copies()
	if err != nil {
		return err
	}
	if len(names) > 0 {
		if latest, err := os.ReadFile(filepath.Join(b.Dir, names[0])); err == nil && bytes.Equal(latest, data) {
			return nil
		}
	}
	name := prefix + now.UTC().Format(stamp) + suffix
	if err := layout.WriteFileAtomic(filepath.Join(b.Dir, name), data, 0o640); err != nil {
		return err
	}
	b.Log.Info("clips backed up", "file", filepath.Join(b.Dir, name))
	names = append([]string{name}, names...)
	for _, old := range names[min(len(names), max(b.Keep, 1)):] {
		if err := os.Remove(filepath.Join(b.Dir, old)); err != nil {
			return err
		}
	}
	return nil
}

// Status describes the backups for the UI.
func (b *Backup) Status() Status {
	b.mu.Lock()
	st := Status{Dir: b.Dir, LastRun: b.lastRun}
	if b.lastErr != nil {
		st.Error = b.lastErr.Error()
	}
	b.mu.Unlock()
	if names, err := b.copies(); err == nil {
		st.Copies = len(names)
		if len(names) > 0 {
			st.Latest, _ = time.Parse(stamp, strings.TrimSuffix(strings.TrimPrefix(names[0], prefix), suffix))
		}
	} else if st.Error == "" {
		st.Error = err.Error()
	}
	return st
}
