package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/egress"
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

// sandboxdCheck asks the broker over its own socket whether it serves sandboxes, for the sandboxd container's
// HEALTHCHECK, so the control plane starts only once it can really isolate work.
func sandboxdCheck(env func(string) (string, bool), stderr io.Writer) int {
	socket := defaultBrokerSocket
	if v, ok := env("OCTOMUS_SANDBOXD_SOCKET"); ok && v != "" {
		socket = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sandbox.NewRemote(socket).Info(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// runEgress serves the egress gateway on the sandbox networks, logging every decision as a JSON line.
func runEgress(env func(string) (string, bool), stdout, stderr io.Writer) error {
	value := func(key, fallback string) string {
		if v, ok := env(key); ok && v != "" {
			return v
		}
		return fallback
	}
	leases := value("OCTOMUS_EGRESS_LEASES", "")
	if leases == "" {
		return errors.New("OCTOMUS_EGRESS_LEASES is required")
	}
	policy := egress.Policy{}
	for key, rules := range map[string]*[]egress.Rule{"OCTOMUS_EGRESS_MODEL_HOSTS": &policy.Model, "OCTOMUS_EGRESS_BUILD_HOSTS": &policy.Build} {
		parsed, err := egress.ParseRules(value(key, ""))
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*rules = parsed
	}
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()
	gateway := egress.New(policy, leases, stdout)
	listen := value("OCTOMUS_EGRESS_LISTEN", ":3128")
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if collector := value("OCTOMUS_EGRESS_COLLECTOR", ""); collector != "" {
		if err := os.Remove(collector); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		old := syscall.Umask(0o177)
		socket, err := net.Listen("unix", collector)
		syscall.Umask(old)
		if err != nil {
			return err
		}
		go func() { _ = gateway.ServeCollector(ctx, socket) }()
	}
	described := policy.Describe()
	fmt.Fprintf(stderr, "Octomus egress gateway on %s; model hosts %v; build hosts %v\n", listen, described["model"], described["build"])
	server := &http.Server{Handler: gateway, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
	cfg.Log = stderr
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
