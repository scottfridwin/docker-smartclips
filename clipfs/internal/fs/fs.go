package fs

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"

	"clipfs/config"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type Root struct {
	fs.Inode
	clips []config.Clip
}

func NewRoot(clips []config.Clip) *Root {
	return &Root{clips: clips}
}

func (r *Root) OnAdd(ctx context.Context) {
	for _, clip := range r.clips {
		file := &VirtualFile{
			name: clip.Output,
			clip: clip,
		}

		inode := r.NewInode(ctx, file, fs.StableAttr{
			Mode: syscall.S_IFREG,
		})

		r.AddChild(clip.Output, inode, false)
	}
}

// --------------------
// Virtual File
// --------------------

type VirtualFile struct {
	fs.Inode

	name string
	clip config.Clip

	mu   sync.Mutex
	data []byte
	once bool
}

func (f *VirtualFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Generate once per lifecycle (simple cache)
	if !f.once {
		data, err := f.generate()
		if err != nil {
			return nil, 0, syscall.EIO
		}

		f.data = data
		f.once = true
	}

	return f, 0, 0
}

func (f *VirtualFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if off >= int64(len(f.data)) {
		return fuse.ReadResultData(nil), 0
	}

	end := int(off) + len(dest)
	if end > len(f.data) {
		end = len(f.data)
	}

	return fuse.ReadResultData(f.data[off:end]), 0
}

// --------------------
// ffmpeg generation
// --------------------

func (f *VirtualFile) generate() ([]byte, error) {
	cmd := exec.Command(
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-ss", formatTime(f.clip.Start),
		"-to", formatTime(f.clip.End),
		"-i", f.clip.Input,
		"-c", "copy",
		"-f", "matroska",
		"pipe:1",
	)

	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		log.Printf("ffmpeg error for %s: %s", f.name, errBuf.String())
		return nil, err
	}

	return out.Bytes(), nil
}

func formatTime(seconds float64) string {
	return fmt.Sprintf("%.2f", seconds)
}

// --------------------
// Metadata
// --------------------

func (f *VirtualFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.mu.Lock()
	if f.once {
		out.Size = uint64(len(f.data))
	} else {
		// Report a large size so readers don't refuse to open a 0-byte file
		out.Size = 1 << 62
	}
	f.mu.Unlock()
	out.Mode = syscall.S_IFREG | 0444

	puid := parseEnvInt("PUID", 0)
	pgid := parseEnvInt("PGID", 0)

	out.Uid = uint32(puid)
	out.Gid = uint32(pgid)

	return 0
}

func parseEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	i, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return i
}
