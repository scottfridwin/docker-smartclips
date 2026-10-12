//go:build linux

// Package privs drops root after the FUSE mount is set up.
package privs

import (
	"fmt"
	"syscall"
)

// Drop switches every thread to uid:gid with gid as the only group. Root stays the saved
// set-user-ID so Elevate can regain it for unmounting; nothing else runs as root.
func Drop(uid, gid int) error {
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("setresgid: %w", err)
	}
	if err := syscall.Setresuid(uid, uid, 0); err != nil {
		return fmt.Errorf("setresuid: %w", err)
	}
	return nil
}

// Elevate restores root as the effective user (used only to unmount at shutdown).
func Elevate() error {
	return syscall.Setresuid(-1, 0, -1)
}

// As runs fn with effective IDs uid:gid, then switches back to root.
func As(uid, gid int, fn func() error) error {
	if err := syscall.Setresgid(-1, gid, -1); err != nil {
		return fmt.Errorf("setresgid: %w", err)
	}
	if err := syscall.Setresuid(-1, uid, -1); err != nil {
		_ = syscall.Setresgid(-1, 0, -1)
		return fmt.Errorf("setresuid: %w", err)
	}
	err := fn()
	if e := syscall.Setresuid(-1, 0, -1); e != nil {
		return fmt.Errorf("restoring root: %w", e)
	}
	if e := syscall.Setresgid(-1, 0, -1); e != nil {
		return fmt.Errorf("restoring root group: %w", e)
	}
	return err
}
