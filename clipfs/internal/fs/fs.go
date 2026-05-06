package fs

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"clipfs/config"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const evictionTTL = 5 * time.Minute

type Root struct {
	fs.Inode
	clips []config.Clip
	uid   uint32
	gid   uint32
}

func NewRoot(clips []config.Clip, uid, gid int) *Root {
	return &Root{
		clips: clips,
		uid:   uint32(uid),
		gid:   uint32(gid),
	}
}

func (r *Root) OnAdd(ctx context.Context) {
	for _, clip := range r.clips {
		file := &VirtualFile{
			name: clip.Output,
			clip: clip,
			root: r,
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
	root *Root

	mu         sync.Mutex
	data       atomic.Pointer[[]byte] // lock-free reads after generation
	generated  atomic.Bool
	lastAccess atomic.Int64 // unix timestamp of last read
}

func (f *VirtualFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if err := f.ensureGenerated(); err != nil {
		return nil, 0, syscall.EIO
	}
	f.touch()
	return f, 0, 0
}

func (f *VirtualFile) ensureGenerated() error {
	if f.generated.Load() {
		return nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Double-check after acquiring lock
	if f.generated.Load() {
		return nil
	}

	data, err := f.generate()
	if err != nil {
		return err
	}

	f.data.Store(&data)
	f.generated.Store(true)
	f.touch()

	// Start eviction timer
	go f.evictionLoop()

	return nil
}

func (f *VirtualFile) touch() {
	f.lastAccess.Store(time.Now().Unix())
}

func (f *VirtualFile) evictionLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		if !f.generated.Load() {
			return
		}
		lastAccess := time.Unix(f.lastAccess.Load(), 0)
		if time.Since(lastAccess) > evictionTTL {
			f.mu.Lock()
			f.data.Store(nil)
			f.generated.Store(false)
			f.mu.Unlock()
			log.Printf("Evicted cached data for %s (idle > %v)", f.name, evictionTTL)
			return
		}
	}
}

func (f *VirtualFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.touch()

	dataPtr := f.data.Load()
	if dataPtr == nil {
		return fuse.ReadResultData(nil), syscall.EIO
	}
	data := *dataPtr

	if off >= int64(len(data)) {
		return fuse.ReadResultData(nil), 0
	}

	end := int(off) + len(dest)
	if end > len(data) {
		end = len(data)
	}

	return fuse.ReadResultData(data[off:end]), 0
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
	if f.generated.Load() {
		dataPtr := f.data.Load()
		if dataPtr != nil {
			out.Size = uint64(len(*dataPtr))
		} else {
			out.Size = 1 << 62
		}
	} else {
		out.Size = 1 << 62
	}

	out.Mode = syscall.S_IFREG | 0444
	out.Uid = f.root.uid
	out.Gid = f.root.gid

	return 0
}
