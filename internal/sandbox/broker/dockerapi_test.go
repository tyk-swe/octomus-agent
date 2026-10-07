package broker

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestAttachHandshakeHonoursCancellation(t *testing.T) {
	listener, socket := testutil.ListenUnix(t, "docker.sock")
	defer listener.Close()
	received := make(chan error, 1)
	// A daemon that reads the attach request and never answers the upgrade.
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			received <- err
			return
		}
		defer conn.Close()
		_, err = http.ReadRequest(bufio.NewReader(conn))
		received <- err
		if err == nil {
			buffer := make([]byte, 1)
			_, _ = conn.Read(buffer)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := newDockerClient(socket).containerAttach(ctx, "silent", true)
		done <- err
	}()
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attach request was never received")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled attach = %v; want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attach handshake ignored cancellation")
	}
}
