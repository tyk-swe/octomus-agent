package broker

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maxCABundle = 1 << 20

// readCABundle accepts certificates only, never private keys. The broker configuration supplies this file; a runner
// cannot select it through the request environment. Refuse symlinks and special files before reading any contents.
func readCABundle(path string, requireCA bool) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("CA bundle must be a canonical absolute path without symlinks")
	}
	// Open each component without following links, so a replaced ancestor cannot redirect the final open either.
	parent, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(parent, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(parent)
		if openErr != nil {
			return nil, openErr
		}
		parent = next
	}
	fd, err := unix.Openat(parent, parts[len(parts)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	unix.Close(parent)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCABundle {
		return nil, errors.New("CA bundle must be a regular file of at most 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCABundle+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCABundle {
		return nil, errors.New("CA bundle exceeds 1 MiB")
	}
	remaining, count := bytes.TrimSpace(data), 0
	for len(remaining) > 0 {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("CA bundle may contain only PEM certificates")
		}
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("CA bundle contains malformed PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || (requireCA && !cert.IsCA) {
			return nil, errors.New("CA bundle contains an invalid CA certificate")
		}
		count++
		remaining = bytes.TrimSpace(rest)
	}
	if count == 0 {
		return nil, errors.New("CA bundle contains no certificates")
	}
	return append(bytes.TrimSpace(data), '\n'), nil
}

// installCA preserves the broker image's system trust when a deployment adds a private provider CA. Both files are public
// certificates, installed with the helper in the tools volume, which every sandbox mounts read-only.
func installCA(customPath, systemPath, dir string) error {
	name := filepath.Base(toolsCAFile)
	if customPath == "" {
		err := os.Remove(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	custom, err := readCABundle(customPath, true)
	if err != nil {
		return fmt.Errorf("custom CA bundle: %w", err)
	}
	// Existing system trust anchors can predate X.509 CA constraints; preserve those explicit anchors.
	system, err := readCABundle(systemPath, false)
	if err != nil {
		return fmt.Errorf("system CA bundle: %w", err)
	}
	return install(dir, name, append(system, custom...), 0o644)
}
