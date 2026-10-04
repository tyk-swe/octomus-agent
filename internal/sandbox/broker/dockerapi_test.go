package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestAttachHandshakeHonoursCancellation(t *testing.T) {
	listener, socket := testutil.ListenUnix(t, "docker.sock")
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
		_, err := newDockerClient(socket).containerAttach(ctx, "silent", true)
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
