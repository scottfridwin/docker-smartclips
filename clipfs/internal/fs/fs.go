package fs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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

func formatTime(seconds float64) string {
	return fmt.Sprintf("%.2f", seconds)
}

type VirtualFile struct {
	fs.Inode
	name string
	clip config.Clip
}

func (f *VirtualFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return f, 0, 0
}

func (f *VirtualFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {

	absoluteStart := f.clip.Start + float64(off)/1024.0 // crude mapping

	cmd := exec.Command(
		"ffmpeg",
		"-ss", fmt.Sprintf("%.2f", absoluteStart),
		"-to", fmt.Sprintf("%.2f", f.clip.End),
		"-i", f.clip.Input,
		"-c", "copy",
		"-f", "matroska",
		"pipe:1",
	)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		return fuse.ReadResultData(nil), syscall.EIO
	}

	data := stdout.Bytes()

	if off >= int64(len(data)) {
		return fuse.ReadResultData(nil), 0
	}

	end := int(off) + len(dest)
	if end > len(data) {
		end = len(data)
	}

	return fuse.ReadResultData(data[off:end]), 0
}

func (f *VirtualFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Size = f.fileSize()
	out.Mode = syscall.S_IFREG | 0444
	return 0
}

func (f *VirtualFile) fileSize() uint64 {
	// very rough estimate:
	// assume 1MB per minute (~placeholder for Jellyfin timeline)
	duration := f.clip.End - f.clip.Start
	return uint64(duration * 1024 * 1024 / 60)
}
