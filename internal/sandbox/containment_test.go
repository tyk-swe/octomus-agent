package sandbox

import (
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

const (
	writableRoot = "1015 980 0:77 / / rw,relatime master:500 - overlay overlay rw,lowerdir=/l\n" +
		"1016 1015 0:80 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw\n"
	readOnlyRoot = "1015 980 0:77 / / ro,relatime master:500 - overlay overlay rw,lowerdir=/l\n" +
		"1017 1015 0:81 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw,size=65536k\n" +
		"1018 1015 8:1 /hosts /etc/hosts rw,relatime - ext4 /dev/sda1 rw\n"
	// gVisor's root mount, under which a write is refused for permission before the read-only mount is consulted.
	gVisorRoot = "1 0 0:1 / / ro - 9p none ro,trans=fd,rfdno=4,wfdno=4\n"
	// A writable mount over /usr hides under a read-only root from a probe that cannot write root-owned directories.
	writableUsr = readOnlyRoot + "1019 1015 0:90 / /usr rw,relatime - overlay overlay rw\n"
)

// A probe that runs as a non-root user cannot write to root-owned /usr, /etc or / on a writable image either. Only
// the ro option of the mount that holds each directory proves it read-only, and a refusal other than EROFS counts only
// under such a mount.
func TestReadOnlyImageNeedsReadOnlyMounts(t *testing.T) {
	refuse := func(errno syscall.Errno) func(string) error {
		return func(path string) error { return &fs.PathError{Op: "open", Path: path, Err: errno} }
	}
	mountinfo := func(text string) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if path != "/proc/self/mountinfo" {
				return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
			}
			return []byte(text), nil
		}
	}
	denied := func(path string) ([]byte, error) {
		return nil, &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}
	}
	cases := []struct {
		name   string
		read   func(string) ([]byte, error)
		write  func(string) error
		passed bool
		detail string
	}{
		{"read-only image", mountinfo(readOnlyRoot), refuse(syscall.EROFS), true, "read-only"},
		{"writable image, non-root probe", mountinfo(writableRoot), refuse(syscall.EACCES), false, "mount / rw,relatime"},
		{"gVisor refuses for permission first", mountinfo(gVisorRoot), refuse(syscall.EACCES), true, "read-only"},
		{"writable mount over /usr", mountinfo(writableUsr), refuse(syscall.EACCES), false, "not proven read-only: /usr refused without EROFS (permission denied) under mount /usr rw,relatime"},
		{"ro mount with another refusal", mountinfo(readOnlyRoot), refuse(syscall.EIO), false, "/usr refused without EROFS (input/output error)"},
		{"no root mount", mountinfo(""), refuse(syscall.EROFS), false, "no root mount in mountinfo"},
		{"unreadable mountinfo", denied, refuse(syscall.EROFS), false, "not proven read-only: mountinfo unreadable (permission denied)"},
		{"writable directories", mountinfo(writableRoot), func(string) error { return nil }, false, "writable /usr, writable /etc, writable /"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			passed, detail := readOnlyImage(c.read, c.write)
			if passed != c.passed || !strings.Contains(detail, c.detail) {
				t.Fatalf("readOnlyImage = %v, %q; want %v with %q", passed, detail, c.passed, c.detail)
			}
		})
	}
	written := []string{}
	readOnlyImage(mountinfo(readOnlyRoot), func(path string) error { written = append(written, path); return syscall.EROFS })
	if strings.Join(written, " ") != "/usr/.octomus-probe /etc/.octomus-probe /.octomus-probe" {
		t.Fatalf("probe wrote %v", written)
	}
}
