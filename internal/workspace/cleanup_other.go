//go:build !linux

package workspace

import (
	"context"
	"errors"

	"golang.org/x/sys/unix"
)

func writableDirectory(ctx context.Context, parent int, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var meta unix.Stat_t
	if err := unix.Fstatat(parent, name, &meta, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if meta.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("cleanup directory changed during permission repair")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return unix.Fchmodat(parent, name, uint32(meta.Mode)&0o777|0o700, unix.AT_SYMLINK_NOFOLLOW)
}
