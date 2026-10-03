package broker

import (
	"io"
	"testing"
)

// brokerOn builds a broker against a test engine without New's deployment checks; limit, when set, caps its
// sandboxes.
func brokerOn(t *testing.T, socket string, limit int) *Broker {
	t.Helper()
	cfg := testConfig(t)
	cfg.DockerSocket, cfg.Log = socket, io.Discard
	if limit > 0 {
		cfg.Max = limit
	}
	return newBroker(cfg)
}
