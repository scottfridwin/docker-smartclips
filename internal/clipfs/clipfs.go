// Package clipfs serves clip layouts as a read-only FUSE filesystem.
package clipfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scottfridwin/docker-smartclips/internal/layout"
)

type entry struct {
	layout  *layout.Layout
	nfo     []byte
	nfoTime time.Time
}

// Root is the filesystem root. Clips can be set before and after mounting.
type Root struct {
	fs.Inode

	uid, gid uint32
	created  time.Time
	onStale  func(clipPath string)

	mu      sync.Mutex
	entries map[string]entry // by clip path (no extension)
	mounted bool
}

var (
	_ fs.NodeOnAdder   = (*Root)(nil)
	_ fs.NodeGetattrer = (*Root)(nil)
	_ fs.NodeStatfser  = (*Root)(nil)
)

// New returns a root whose files are owned by uid:gid. onStale is called when a clip's
// data file changed underneath it.
func New(uid, gid int, onStale func(clipPath string)) *Root {
	return &Root{
		uid: uint32(uid), gid: uint32(gid), created: time.Now(),
		onStale: onStale, entries: map[string]entry{},
	}
}

// OnAdd builds the tree from the clips set before mounting.
func (r *Root) OnAdd(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, e := range r.entries {
		r.place(ctx, p, e, false)
	}
	r.mounted = true
}

// SetClip adds or replaces a clip.
func (r *Root) SetClip(clipPath string, l *layout.Layout, nfo []byte, nfoTime time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := entry{layout: l, nfo: nfo, nfoTime: nfoTime}
	r.entries[clipPath] = e
	if r.mounted {
		r.place(context.Background(), clipPath, e, true)
	}
}

// RemoveClip removes a clip and any directories left empty.
func (r *Root) RemoveClip(clipPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[clipPath]; !ok {
		return
	}
	delete(r.entries, clipPath)
	if !r.mounted {
		return
	}
	dir, name := path.Split(clipPath)
	parent := r.lookupDir(dir)
	if parent == nil {
		return
	}
	for _, n := range []string{name + ".mkv", name + ".nfo"} {
		if parent.GetChild(n) != nil {
			parent.RmChild(n)
			_ = parent.NotifyEntry(n)
		}
	}
	r.prune(dir)
}

// Len is the number of clips being served.
func (r *Root) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *Root) lookupDir(dir string) *fs.Inode {
	node := &r.Inode
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		if node = node.GetChild(part); node == nil {
			return nil
		}
	}
	return node
}

// prune removes empty directories from dir upwards.
func (r *Root) prune(dir string) {
	for dir = strings.Trim(dir, "/"); dir != ""; dir = path.Dir(dir) {
		node := r.lookupDir(dir)
		if node == nil || len(node.Children()) > 0 {
			return
		}
		parentDir, name := path.Split(dir)
		parent := r.lookupDir(parentDir)
		if parent == nil {
			return
		}
		parent.RmChild(name)
		_ = parent.NotifyEntry(name)
		if parentDir == "" {
			return
		}
	}
}

func (r *Root) place(ctx context.Context, clipPath string, e entry, notify bool) {
	dir, name := path.Split(clipPath)
	parent := &r.Inode
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		child := parent.GetChild(part)
		if child == nil {
			child = parent.NewPersistentInode(ctx, &dirNode{root: r}, fs.StableAttr{Mode: syscall.S_IFDIR})
			parent.AddChild(part, child, false)
			if notify {
				_ = parent.NotifyEntry(part)
			}
		}
		parent = child
	}
	set := func(n string, node fs.InodeEmbedder) {
		child := parent.NewPersistentInode(ctx, node, fs.StableAttr{Mode: syscall.S_IFREG})
		parent.AddChild(n, child, true)
		if notify {
			_ = parent.NotifyEntry(n)
		}
	}
	set(name+".mkv", &clipNode{root: r, path: clipPath, layout: e.layout})
	if e.nfo != nil {
		set(name+".nfo", &staticNode{root: r, data: e.nfo, mtime: e.nfoTime})
	} else if parent.GetChild(name+".nfo") != nil {
		parent.RmChild(name + ".nfo")
		if notify {
			_ = parent.NotifyEntry(name + ".nfo")
		}
	}
}

func (r *Root) fill(out *fuse.Attr, mode uint32, size uint64, mtime time.Time) {
	out.Mode = mode
	out.Size = size
	out.Uid, out.Gid = r.uid, r.gid
	out.Nlink = 1
	out.SetTimes(&mtime, &mtime, &mtime)
	out.Blksize = 1 << 16
	out.Blocks = (size + 511) / 512
}

// Getattr reports the root directory.
func (r *Root) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	r.fill(&out.Attr, syscall.S_IFDIR|0o555, 0, r.created)
	return 0
}

// Statfs reports an empty, read-only filesystem.
func (r *Root) Statfs(_ context.Context, out *fuse.StatfsOut) syscall.Errno {
	out.Bsize = 1 << 16
	out.NameLen = 255
	return 0
}

type dirNode struct {
	fs.Inode
	root *Root
}

var _ fs.NodeGetattrer = (*dirNode)(nil)

func (d *dirNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	d.root.fill(&out.Attr, syscall.S_IFDIR|0o555, 0, d.root.created)
	return 0
}

type staticNode struct {
	fs.Inode
	root  *Root
	data  []byte
	mtime time.Time
}

var (
	_ fs.NodeGetattrer = (*staticNode)(nil)
	_ fs.NodeOpener    = (*staticNode)(nil)
	_ fs.NodeReader    = (*staticNode)(nil)
)

func (s *staticNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	s.root.fill(&out.Attr, syscall.S_IFREG|0o444, uint64(len(s.data)), s.mtime)
	return 0
}

func (s *staticNode) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (s *staticNode) Read(_ context.Context, _ fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off >= int64(len(s.data)) {
		return fuse.ReadResultData(nil), 0
	}
	return fuse.ReadResultData(s.data[off:min(int64(len(s.data)), off+int64(len(dest)))]), 0
}

type clipNode struct {
	fs.Inode
	root   *Root
	path   string
	layout *layout.Layout
}

var (
	_ fs.NodeGetattrer = (*clipNode)(nil)
	_ fs.NodeOpener    = (*clipNode)(nil)
)

func (c *clipNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	c.root.fill(&out.Attr, syscall.S_IFREG|0o444, uint64(c.layout.Size), time.Unix(0, c.layout.Built))
	return 0
}

func (c *clipNode) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return nil, 0, syscall.EROFS
	}
	f, err := os.Open(c.layout.Data.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && c.root.onStale != nil {
			c.root.onStale(c.path)
		}
		return nil, 0, errno(err)
	}
	if !c.layout.Data.Matches(f) {
		_ = f.Close()
		if c.root.onStale != nil {
			c.root.onStale(c.path)
		}
		return nil, 0, syscall.EIO
	}
	return &clipHandle{f: f, layout: c.layout}, fuse.FOPEN_KEEP_CACHE, 0
}

type clipHandle struct {
	f      *os.File
	layout *layout.Layout
}

var (
	_ fs.FileReader   = (*clipHandle)(nil)
	_ fs.FileReleaser = (*clipHandle)(nil)
)

func (h *clipHandle) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.layout.ReadAt(dest, off, h.f)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func errno(err error) syscall.Errno {
	var e syscall.Errno
	if errors.As(err, &e) {
		return e
	}
	return syscall.EIO
}

func (h *clipHandle) Release(context.Context) syscall.Errno {
	_ = h.f.Close()
	return 0
}
