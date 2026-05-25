package fs

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"smartclips/config"
	"smartclips/internal/cache"
	"smartclips/internal/nfo"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type Root struct {
	fs.Inode
	clips []config.Clip
	uid   uint32
	gid   uint32
	cache *cache.DiskCache
	mu    sync.RWMutex
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
		r.addClip(ctx, clip)
	}
}

// getOrCreateDir walks/creates the directory path under parent, returning the leaf dir inode.
func (r *Root) getOrCreateDir(ctx context.Context, parent *fs.Inode, path string) *fs.Inode {
	parts := strings.Split(path, "/")
	current := parent

	for _, part := range parts {
		if part == "" {
			continue
		}
		child := current.GetChild(part)
		if child == nil {
			dir := &Dir{root: r}
			child = current.NewInode(ctx, dir, fs.StableAttr{Mode: syscall.S_IFDIR})
			current.AddChild(part, child, true)
		}
		current = child
	}

	return current
}

func (r *Root) addClip(ctx context.Context, clip config.Clip) {
	// Determine parent directory
	var parent *fs.Inode
	if clip.Group != "" {
		parent = r.getOrCreateDir(ctx, &r.Inode, clip.Group)
	} else {
		parent = &r.Inode
	}

	// Add the .mkv virtual file
	mkvName := clip.Output + ".mkv"
	file := &VirtualFile{
		name: mkvName,
		clip: clip,
		root: r,
	}
	mkvInode := parent.NewInode(ctx, file, fs.StableAttr{Mode: syscall.S_IFREG})
	parent.AddChild(mkvName, mkvInode, true)

	// Add the .nfo virtual file if metadata is present
	if clip.Metadata != nil {
		nfoName := clip.Output + ".nfo"
		nfoData := nfo.Generate(clip.NfoType, clip.Metadata)
		nfoFile := &StaticFile{
			data: nfoData,
			root: r,
		}
		nfoInode := parent.NewInode(ctx, nfoFile, fs.StableAttr{Mode: syscall.S_IFREG})
		parent.AddChild(nfoName, nfoInode, true)
	}
}

// removeClip removes a clip's .mkv and .nfo from the tree.
func (r *Root) removeClip(clip config.Clip) {
	var parent *fs.Inode
	if clip.Group != "" {
		// Walk to the parent directory
		parent = &r.Inode
		parts := strings.Split(clip.Group, "/")
		for _, part := range parts {
			child := parent.GetChild(part)
			if child == nil {
				return
			}
			parent = child
		}
	} else {
		parent = &r.Inode
	}

	parent.RmChild(clip.Output + ".mkv")
	parent.RmChild(clip.Output + ".nfo")

	// Clean up empty parent directories
	if clip.Group != "" {
		r.pruneEmptyDirs(&r.Inode, strings.Split(clip.Group, "/"))
	}
}

// pruneEmptyDirs removes empty directories bottom-up.
func (r *Root) pruneEmptyDirs(parent *fs.Inode, parts []string) {
	if len(parts) == 0 {
		return
	}

	child := parent.GetChild(parts[0])
	if child == nil {
		return
	}

	if len(parts) > 1 {
		r.pruneEmptyDirs(child, parts[1:])
	}

	// After recursion, check if child is now empty
	if len(child.Children()) == 0 {
		parent.RmChild(parts[0])
	}
}

// Reload updates the FUSE tree to match the new clip list.
func (r *Root) Reload(clips []config.Clip) {
	r.mu.Lock()
	defer r.mu.Unlock()

	newSet := make(map[string]config.Clip, len(clips))
	for _, c := range clips {
		newSet[c.FullPath()] = c
	}

	oldSet := make(map[string]config.Clip, len(r.clips))
	for _, c := range r.clips {
		oldSet[c.FullPath()] = c
	}

	// Remove clips that no longer exist
	for path, clip := range oldSet {
		if _, exists := newSet[path]; !exists {
			r.removeClip(clip)
			log.Printf("Reload: removed %s", path)
		}
	}

	// Add new clips
	ctx := context.Background()
	for path, clip := range newSet {
		if _, exists := oldSet[path]; !exists {
			r.addClip(ctx, clip)
			log.Printf("Reload: added %s", path)
		}
	}

	r.clips = clips
	log.Printf("Reload: %d clips active", len(clips))
}

// --------------------
// Dir - virtual directory node
// --------------------

type Dir struct {
	fs.Inode
	root *Root
}

func (d *Dir) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0555
	out.Uid = d.root.uid
	out.Gid = d.root.gid
	return 0
}

// --------------------
// StaticFile - in-memory virtual file (for .nfo)
// --------------------

type StaticFile struct {
	fs.Inode
	data []byte
	root *Root
}

func (f *StaticFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *StaticFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off >= int64(len(f.data)) {
		return fuse.ReadResultData(nil), 0
	}
	end := int(off) + len(dest)
	if end > len(f.data) {
		end = len(f.data)
	}
	return fuse.ReadResultData(f.data[off:end]), 0
}

func (f *StaticFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Size = uint64(len(f.data))
	out.Mode = syscall.S_IFREG | 0444
	out.Uid = f.root.uid
	out.Gid = f.root.gid
	return 0
}

// --------------------
// VirtualFile - ffmpeg-generated clip file
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

	f.mu.RLock()
	path := f.cachePath
	f.mu.RUnlock()

	fh, err := NewCacheFileHandle(path)
	if err != nil {
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

	if f.ready {
		if path, size, ok := f.root.cache.Get(f.cacheKey()); ok {
			f.cachePath = path
			f.cacheSize = size
			return nil
		}
	}

	outPath := f.root.cache.PathFor(f.cacheKey())
	if err := f.generate(outPath); err != nil {
		os.Remove(outPath)
		return err
	}

	path, err := f.root.cache.Admit(f.cacheKey())
	if err != nil {
		return err
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cache: stat admitted file %s: %w", path, err)
	}
	f.cachePath = path
	f.cacheSize = info.Size()
	f.ready = true
	return nil
}

// --------------------
// CacheFileHandle
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
		out.Size = 1 << 62
	}
	f.mu.RUnlock()

	out.Mode = syscall.S_IFREG | 0444
	out.Uid = f.root.uid
	out.Gid = f.root.gid

	return 0
}
