package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// prepareDeployment reads the Docker deployment's fixed settings. A GitHub token or pinned repository only exists in
// Docker mode: without a sandbox, runners would inherit the one and could rewrite the other.
func prepareDeployment(ctx context.Context, mode sandbox.Mode, data string, env func(string) (string, bool), stderr io.Writer) (engine.Deployment, error) {
	repo, pinned := env("OCTOMUS_GITHUB_REPO")
	_, token := env(githubTokenEnv)
	fromFile, _ := env("OCTOMUS_GITHUB_TOKEN_FILE")
	if mode == sandbox.ModeOff {
		if pinned && repo != "" {
			return engine.Deployment{}, errors.New("OCTOMUS_GITHUB_REPO needs --sandbox docker; unsandboxed runners could rewrite a managed checkout")
		}
		if fromFile != "" {
			return engine.Deployment{}, errors.New("OCTOMUS_GITHUB_TOKEN_FILE needs --sandbox docker; unsandboxed runners would inherit the token")
		}
		return engine.Deployment{}, nil
	}
	policy, err := egress.PolicyFromEnv(getenv(env))
	if err != nil {
		return engine.Deployment{}, err
	}
	deployment := engine.Deployment{Egress: policy.Describe()}
	if !pinned || repo == "" {
		return deployment, nil
	}
	if !config.ValidGitHubRepo(repo) {
		return engine.Deployment{}, errors.New("OCTOMUS_GITHUB_REPO must be OWNER/REPOSITORY")
	}
	checkout := filepath.Join(data, "checkout")
	deployment.Repository, deployment.GitHubRepo = checkout, repo
	if _, err := os.Stat(filepath.Join(checkout, ".git")); err == nil {
		if err := checkoutValid(ctx, checkout, repo); err != nil {
			if errors.As(err, new(gitops.AuthError)) {
				return engine.Deployment{}, fmt.Errorf("GitHub authentication failed for %s; check OCTOMUS_GITHUB_TOKEN_FILE and network access: %w", repo, err)
			}
			return engine.Deployment{}, fmt.Errorf("The trusted checkout is not %s; remove %s and restart: %w", repo, checkout, err)
		}
		return deployment, nil
	}
	if !token {
		return engine.Deployment{}, errors.New("The trusted checkout needs a GitHub token; set OCTOMUS_GITHUB_TOKEN_FILE")
	}
	fmt.Fprintf(stderr, "Cloning %s into the trusted checkout; later starts reuse it.\n", repo)
	partial := checkout + ".partial"
	_ = os.RemoveAll(partial)
	if err := gitops.CloneRemote(ctx, "https://github.com/"+repo+".git", partial, 3600); err != nil {
		_ = os.RemoveAll(partial)
		return engine.Deployment{}, fmt.Errorf("Cloning %s: %w", repo, err)
	}
	if err := os.Rename(partial, checkout); err != nil {
		return engine.Deployment{}, err
	}
	if err := checkoutValid(ctx, checkout, repo); err != nil {
		return engine.Deployment{}, fmt.Errorf("The new checkout does not verify: %w", err)
	}
	return deployment, nil
}

// checkoutValid checks that the trusted checkout's origin is the pinned repository and that gh is signed in.
func checkoutValid(ctx context.Context, checkout, repo string) error {
	cfg := config.Default()
	cfg.Repository, cfg.GitHubRepo = checkout, repo
	return gitops.ValidateRemote(ctx, cfg)
}

// healthcheck asks this container's own service whether it is up, for Docker's HEALTHCHECK.
func healthcheck(listen string, stderr io.Writer) int {
	address, err := parseListen(listen)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	host := address.Addr().Unmap()
	if host.IsUnspecified() {
		if host.Is4() {
			host = netip.AddrFrom4([4]byte{127, 0, 0, 1})
		} else {
			host = netip.IPv6Loopback()
		}
	}
	target := url.URL{Scheme: "http", Host: netip.AddrPortFrom(host, address.Port()).String(), Path: "/healthz"}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get(target.String())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
