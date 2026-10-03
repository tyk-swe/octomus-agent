package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheckReachesTheConfiguredListener(t *testing.T) {
	for _, tc := range []struct{ network, address string }{
		{"tcp4", "127.0.0.1:0"},
		{"tcp4", "127.0.0.2:0"},
		{"tcp4", "0.0.0.0:0"},
		{"tcp6", "[::1]:0"},
		{"tcp6", "[::]:0"},
	} {
		t.Run(tc.address, func(t *testing.T) {
			listener, err := net.Listen(tc.network, tc.address)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			server.Listener.Close()
			server.Listener = listener
			server.Start()
			t.Cleanup(server.Close)

			for _, source := range []string{"flag", "environment"} {
				args := []string{"--healthcheck"}
				if source == "flag" {
					args = append(args, "--listen", listener.Addr().String())
				}
				env := func(key string) (string, bool) {
					if source == "environment" && key == "OCTOMUS_LISTEN" {
						return listener.Addr().String(), true
					}
					return "", false
				}
				var stdout, stderr bytes.Buffer
				if code := run(args, env, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("healthy listener %s from %s: exit=%d stdout=%q stderr=%q", listener.Addr(), source, code, stdout.String(), stderr.String())
				}
			}
		})
	}
}
