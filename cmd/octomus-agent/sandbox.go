package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/broker"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// sandboxBackend picks where untrusted children run. Off is an explicit choice for a dedicated VM and says so.
func sandboxBackend(mode sandbox.Mode, env func(string) (string, bool), stderr io.Writer) sandbox.Backend {
	if mode == sandbox.ModeOff {
		fmt.Fprintln(stderr, "Sandbox is off: runners and verification commands run with this service user's permissions. Use only on a dedicated VM.")
		return sandbox.Host{}
	}
	return sandbox.NewRemote(brokerSocket(env))
}

// brokerSocket is the broker's socket the control plane dials.
func brokerSocket(env func(string) (string, bool)) string {
	if v, ok := env("OCTOMUS_SANDBOXD_SOCKET"); ok && v != "" {
		return v
	}
	return wire.DefaultSocket
}

// getenv adapts env to a lookup that reads an unset variable as empty.
func getenv(env func(string) (string, bool)) func(string) string {
	return func(key string) string {
		v, _ := env(key)
		return v
	}
}

// envOr reads key from env, or fallback when it is unset or empty.
func envOr(env func(string) (string, bool), key, fallback string) string {
	if v, ok := env(key); ok && v != "" {
		return v
	}
	return fallback
}

// sandboxdCheck asks the broker over its own socket whether it serves sandboxes, for the sandboxd container's
// HEALTHCHECK, so the control plane starts only once it can really isolate work.
func sandboxdCheck(env func(string) (string, bool), stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sandbox.NewRemote(brokerSocket(env)).Info(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// runEgress serves the egress gateway on the sandbox networks, logging tunnels and refusals as JSON lines.
func runEgress(env func(string) (string, bool), stdout, stderr io.Writer) error {
	leases := envOr(env, "OCTOMUS_EGRESS_LEASES", "")
	if leases == "" {
		return errors.New("OCTOMUS_EGRESS_LEASES is required")
	}
	policy, err := egress.PolicyFromEnv(getenv(env))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()
	gateway := egress.New(policy, leases, stdout)
	listen := envOr(env, "OCTOMUS_EGRESS_LISTEN", ":3128")
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if collector := envOr(env, "OCTOMUS_EGRESS_COLLECTOR", ""); collector != "" {
		if err := os.Remove(collector); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		old := syscall.Umask(0o177)
		socket, err := net.Listen("unix", collector)
		syscall.Umask(old)
		if err != nil {
			return err
		}
		go func() {
			if err := gateway.ServeCollector(ctx, socket); err != nil {
				fmt.Fprintf(stderr, "Octomus egress collector stopped; the broker marks sandbox records incomplete until the gateway restarts: %v\n", err)
			}
		}()
	}
	described := policy.Describe()
	fmt.Fprintf(stderr, "Octomus egress gateway on %s; model hosts %v; build hosts %v\n", listen, described["model"], described["build"])
	return gateway.Serve(ctx, listener)
}

// loginLease grants a runner login its egress lease, as the broker grants a runner sandbox's, and writes the proxy URL
// the login reads. It first revokes the previous login's lease, whose credential is in the file it replaces, so only
// the latest login can reach out.
func loginLease(env func(string) (string, bool)) error {
	for _, key := range []string{"OCTOMUS_EGRESS_LEASES", "OCTOMUS_EGRESS_PROXY", "OCTOMUS_LOGIN_PROXY_FILE"} {
		if envOr(env, key, "") == "" {
			return fmt.Errorf("%s is required", key)
		}
	}
	leases := egress.Leases{Dir: envOr(env, "OCTOMUS_EGRESS_LEASES", "")}
	file := envOr(env, "OCTOMUS_LOGIN_PROXY_FILE", "")
	if previous, err := os.ReadFile(file); err == nil {
		if address, err := url.Parse(strings.TrimSpace(string(previous))); err == nil {
			if token, ok := address.User.Password(); ok {
				leases.Revoke(token)
			}
		}
	}
	token, err := leases.Grant("login", wire.KindRunner)
	if err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(egress.ProxyURL(envOr(env, "OCTOMUS_EGRESS_PROXY", ""), token)+"\n"), 0o600); err != nil {
		leases.Revoke(token)
		return err
	}
	return nil
}

// runBroker serves the sandbox broker until a shutdown signal, then removes every sandbox it started.
func runBroker(env func(string) (string, bool), stderr io.Writer) error {
	cfg, err := broker.LoadConfig(getenv(env))
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
