package workspace

import (
	"context"
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// writableDirectory also handles mode 000, which cannot be opened for reading by its owner. An O_PATH descriptor
// pins the directory without following a symlink. Older kernels need the same proc-fd chmod fallback Go's os.Root
// uses when fchmodat2 is unavailable; the descriptor stays open throughout, so no untrusted path is resolved there.
func writableDirectory(ctx context.Context, parent int, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Openat(parent, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := ctx.Err(); err != nil {
		return err
	}
	var meta unix.Stat_t
	if err := unix.Fstat(fd, &meta); err != nil {
		return err
	}
	mode := meta.Mode&0o777 | 0o700
	if err := ctx.Err(); err != nil {
		return err
	}
	err = unix.Fchmodat(fd, "", mode, unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.EOPNOTSUPP) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return os.Chmod("/proc/self/fd/"+strconv.Itoa(fd), os.FileMode(mode))
	}
	return err
}
