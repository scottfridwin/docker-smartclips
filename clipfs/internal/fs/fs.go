package fs

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"clipfs/config"
	"clipfs/internal/cache"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type Root struct {
	fs.Inode
	clips []config.Clip
	uid   uint32
	gid   uint32
	cache *cache.DiskCache
}

func NewRoot(clips []config.Clip, uid, gid int, diskCache *cache.DiskCache) *Root {
	return &Root{
		clips: clips,
		uid:   uint32(uid),
		gid:   uint32(gid),
		cache: diskCache,
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

	mu       sync.Mutex
	cachePath string
	cacheSize int64
	ready     bool
}

func (f *VirtualFile) cacheKey() string {
	return fmt.Sprintf("%s:%.2f-%.2f", f.clip.Input, f.clip.Start, f.clip.End)
}

func (f *VirtualFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if err := f.ensureCached(); err != nil {
		return nil, 0, syscall.EIO
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *VirtualFile) ensureCached() error {
	// Fast path: already cached
	if f.ready {
		if path, size, ok := f.root.cache.Get(f.cacheKey()); ok {
			f.cachePath = path
			f.cacheSize = size
			return nil
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Double-check after lock
	if path, size, ok := f.root.cache.Get(f.cacheKey()); ok {
		f.cachePath = path
		f.cacheSize = size
		f.ready = true
		return nil
	}

	data, err := f.generate()
	if err != nil {
		return err
	}

	path, err := f.root.cache.Put(f.cacheKey(), data)
	if err != nil {
		return err
	}

	f.cachePath = path
	f.cacheSize = int64(len(data))
	f.ready = true
	return nil
}

func (f *VirtualFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if !f.ready {
		return fuse.ReadResultData(nil), syscall.EIO
	}

	// Read directly from disk cache file
	file, err := os.Open(f.cachePath)
	if err != nil {
		// Cache file was evicted between open and read; regenerate
		f.ready = false
		if err := f.ensureCached(); err != nil {
			return fuse.ReadResultData(nil), syscall.EIO
		}
		file, err = os.Open(f.cachePath)
		if err != nil {
			return fuse.ReadResultData(nil), syscall.EIO
		}
	}
	defer file.Close()

	if off >= f.cacheSize {
		return fuse.ReadResultData(nil), 0
	}

	end := int64(len(dest))
	if off+end > f.cacheSize {
		end = f.cacheSize - off
	}

	buf := make([]byte, end)
	n, _ := file.ReadAt(buf, off)
	return fuse.ReadResultData(buf[:n]), 0
}

// --------------------
// ffmpeg generation
// --------------------

func (f *VirtualFile) generate() ([]byte, error) {
	log.Printf("Generating clip: %s", f.name)

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
	if f.ready {
		out.Size = uint64(f.cacheSize)
	} else {
		// Large dummy size so readers don't bail on 0 bytes
		out.Size = 1 << 62
	}

	out.Mode = syscall.S_IFREG | 0444
	out.Uid = f.root.uid
	out.Gid = f.root.gid

	return 0
}
