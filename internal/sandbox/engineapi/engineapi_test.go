package engineapi

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAttachHandshakeHonoursCancellation(t *testing.T) {
	dir, err := os.MkdirTemp("", "octomus-engine-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// A daemon that accepts the attach connection and never answers the upgrade.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() {
		_, err := New(socket).ContainerAttach(ctx, "silent", true)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled attach = %v; want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attach handshake ignored cancellation")
	}
}
