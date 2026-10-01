package broker

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Listen opens the broker socket, readable and writable only by its owner.
func (b *Broker) Listen() (net.Listener, error) {
	if err := os.Remove(b.cfg.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	old := unix.Umask(0o177)
	listener, err := net.Listen("unix", b.cfg.Socket)
	unix.Umask(old)
	if err != nil {
		return nil, err
	}
	return &peerListener{Listener: listener, uid: b.cfg.ClientUID}, nil
}

// peerListener serves only connections from the configured client uid, checked by the kernel's peer credentials.
type peerListener struct {
	net.Listener
	uid int
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if uid, ok := peerUID(conn); ok && uid == l.uid {
			return conn, nil
		}
		conn.Close()
	}
}

func peerUID(conn net.Conn) (int, bool) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return 0, false
	}
	return int(cred.Uid), true
}
