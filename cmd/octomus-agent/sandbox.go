package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/broker"
)

const defaultBrokerSocket = "/run/octomus/sandboxd.sock"

// sandboxBackend picks where untrusted children run. Off is an explicit choice for a dedicated VM and says so.
func sandboxBackend(mode sandbox.Mode, env func(string) (string, bool), stderr io.Writer) sandbox.Backend {
	if mode == sandbox.ModeOff {
		fmt.Fprintln(stderr, "Sandbox is off: runners and verification commands run with this service user's permissions. Use only on a dedicated VM.")
		return sandbox.Host{}
	}
	socket := defaultBrokerSocket
	if v, ok := env("OCTOMUS_SANDBOXD_SOCKET"); ok && v != "" {
		socket = v
	}
	return sandbox.NewRemote(socket)
}

// runBroker serves the sandbox broker until a shutdown signal, then removes every sandbox it started.
func runBroker(env func(string) (string, bool), stderr io.Writer) error {
	cfg, err := broker.LoadConfig(func(key string) string {
		v, _ := env(key)
		return v
	})
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()
	b, err := broker.New(ctx, cfg, executable)
	if err != nil {
		return err
	}
	listener, err := b.Listen()
	if err != nil {
		return err
	}
	info := b.Info()
	fmt.Fprintf(stderr, "Octomus sandbox broker serving %s (image %s, up to %d sandboxes)\n", cfg.Socket, info.ImageID, info.Limits.Max)
	return b.Serve(ctx, listener)
}
