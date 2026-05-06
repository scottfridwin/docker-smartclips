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
		filename := clip.Output + ".mkv"
		file := &VirtualFile{
			name: filename,
			clip: clip,
			root: r,
		}

		inode := r.NewInode(ctx, file, fs.StableAttr{
			Mode: syscall.S_IFREG,
		})

		r.AddChild(filename, inode, false)
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

	mu        sync.RWMutex
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

	// Open a persistent file handle for this session
	f.mu.RLock()
	path := f.cachePath
	f.mu.RUnlock()

	fh, err := NewCacheFileHandle(path)
	if err != nil {
		// Cache may have been evicted; try once more
		f.mu.Lock()
		f.ready = false
		f.mu.Unlock()
		if err := f.ensureCached(); err != nil {
			return nil, 0, syscall.EIO
		}
		f.mu.RLock()
		path = f.cachePath
		f.mu.RUnlock()
		fh, err = NewCacheFileHandle(path)
		if err != nil {
			return nil, 0, syscall.EIO
		}
	}

	return fh, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *VirtualFile) ensureCached() error {
	f.mu.RLock()
	if f.ready {
		_, _, ok := f.root.cache.Get(f.cacheKey())
		f.mu.RUnlock()
		if ok {
			return nil
		}
	} else {
		f.mu.RUnlock()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Double-check after lock
	if f.ready {
		if path, size, ok := f.root.cache.Get(f.cacheKey()); ok {
			f.cachePath = path
			f.cacheSize = size
			return nil
		}
	}

	// Get the target path and have ffmpeg write directly to it
	outPath := f.root.cache.PathFor(f.cacheKey())
	if err := f.generate(outPath); err != nil {
		os.Remove(outPath)
		return err
	}

	// Register the file in the cache (handles LRU eviction)
	path, err := f.root.cache.Admit(f.cacheKey())
	if err != nil {
		return err
	}

	info, _ := os.Stat(path)
	f.cachePath = path
	f.cacheSize = info.Size()
	f.ready = true
	return nil
}

// --------------------
// CacheFileHandle - persistent file handle for reads
// --------------------

type CacheFileHandle struct {
	file *os.File
	size int64
}

func NewCacheFileHandle(path string) (*CacheFileHandle, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &CacheFileHandle{file: file, size: info.Size()}, nil
}

func (fh *CacheFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off >= fh.size {
		return fuse.ReadResultData(nil), 0
	}

	end := int64(len(dest))
	if off+end > fh.size {
		end = fh.size - off
	}

	buf := dest[:end]
	n, _ := fh.file.ReadAt(buf, off)
	return fuse.ReadResultData(buf[:n]), 0
}

func (fh *CacheFileHandle) Release(ctx context.Context) syscall.Errno {
	fh.file.Close()
	return 0
}

// Ensure CacheFileHandle implements the needed interfaces
var _ = (fs.FileReader)((*CacheFileHandle)(nil))
var _ = (fs.FileReleaser)((*CacheFileHandle)(nil))

// --------------------
// ffmpeg generation
// --------------------

func (f *VirtualFile) generate(outPath string) error {
	log.Printf("Generating clip: %s -> %s", f.name, outPath)

	cmd := exec.Command(
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-ss", formatTime(f.clip.Start),
		"-to", formatTime(f.clip.End),
		"-i", f.clip.Input,
		"-c", "copy",
		"-y",
		outPath,
	)

	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		log.Printf("ffmpeg error for %s: %s", f.name, errBuf.String())
		return err
	}

	return nil
}

func formatTime(seconds float64) string {
	return fmt.Sprintf("%.2f", seconds)
}

// --------------------
// Metadata
// --------------------

func (f *VirtualFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.mu.RLock()
	if f.ready {
		out.Size = uint64(f.cacheSize)
	} else {
		// Large dummy size so readers don't bail on 0 bytes
		out.Size = 1 << 62
	}
	f.mu.RUnlock()

	out.Mode = syscall.S_IFREG | 0444
	out.Uid = f.root.uid
	out.Gid = f.root.gid

	return 0
}
