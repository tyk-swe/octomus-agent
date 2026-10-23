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
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// preparedDeployments is the Docker deployment's fixed settings: one base
// (egress only) plus a pinned trusted checkout per repository, keyed by the
// lowercase GitHub identity for engine.FleetOptions.
type preparedDeployments struct {
	base    engine.Deployment
	pins    map[string]engine.Deployment
	primary string
}

// prepareDeployments reads the Docker deployment's fixed settings. A GitHub
// token or pinned repository only exists in Docker mode: without a sandbox,
// runners would inherit the one and could rewrite the other.
// OCTOMUS_GITHUB_REPO pins the primary repository with checkout at
// data/checkout, exactly as before; OCTOMUS_GITHUB_REPOS adds a
// comma-separated list, each with a checkout under data/checkouts/ and its
// own dedicated token (OCTOMUS_GITHUB_TOKEN__OWNER_2FNAME or the tokens dir).
func prepareDeployments(ctx context.Context, mode sandbox.Mode, data string, env func(string) (string, bool), stderr io.Writer) (preparedDeployments, error) {
	repo, pinned := env("OCTOMUS_GITHUB_REPO")
	list, listed := env("OCTOMUS_GITHUB_REPOS")
	fromFile, _ := env("OCTOMUS_GITHUB_TOKEN_FILE")
	if mode == sandbox.ModeOff {
		if (pinned && repo != "") || (listed && list != "") {
			return preparedDeployments{}, errors.New("OCTOMUS_GITHUB_REPO and OCTOMUS_GITHUB_REPOS need --sandbox docker; unsandboxed runners could rewrite a managed checkout")
		}
		if fromFile != "" {
			return preparedDeployments{}, errors.New("OCTOMUS_GITHUB_TOKEN_FILE needs --sandbox docker; unsandboxed runners would inherit the token")
		}
		return preparedDeployments{pins: map[string]engine.Deployment{}}, nil
	}
	policy, err := egress.PolicyFromEnv(getenv(env))
	if err != nil {
		return preparedDeployments{}, err
	}
	out := preparedDeployments{base: engine.Deployment{Egress: policy.Describe()}, pins: map[string]engine.Deployment{}}
	type pin struct {
		repo, checkout string
		dedicated      bool
	}
	pins := []pin{}
	if pinned && repo != "" {
		if !config.ValidGitHubRepo(repo) {
			return preparedDeployments{}, errors.New("OCTOMUS_GITHUB_REPO must be OWNER/REPOSITORY")
		}
		pins = append(pins, pin{repo: repo, checkout: filepath.Join(data, "checkout")})
		out.primary = repo
	}
	if listed {
		for _, candidate := range strings.Split(list, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			if !config.ValidGitHubRepo(candidate) {
				return preparedDeployments{}, errors.New("OCTOMUS_GITHUB_REPOS entries must be OWNER/REPOSITORY")
			}
			duplicate := false
			for _, existing := range pins {
				duplicate = duplicate || config.EqualASCII(existing.repo, candidate)
			}
			if !duplicate {
				pins = append(pins, pin{repo: candidate, checkout: filepath.Join(data, "checkouts", store.RepositorySlug(candidate)), dedicated: true})
			}
		}
	}
	for _, p := range pins {
		if p.dedicated && !config.HasOwnGitHubToken(p.repo) {
			return preparedDeployments{}, fmt.Errorf("Repository %s needs its own token in %s (or a %s file in %s); multi-repository operation never shares the primary token", p.repo, config.RepoGitHubTokenEnv(p.repo), store.RepositorySlug(p.repo), config.GitHubTokensDirEnv)
		}
		deployment := out.base
		deployment.Repository, deployment.GitHubRepo = p.checkout, p.repo
		if _, err := os.Stat(filepath.Join(p.checkout, ".git")); err == nil {
			if err := checkoutValid(ctx, p.checkout, p.repo); err != nil {
				if errors.As(err, new(gitops.AuthError)) {
					return preparedDeployments{}, fmt.Errorf("GitHub authentication failed for %s; check its token and network access: %w", p.repo, err)
				}
				return preparedDeployments{}, fmt.Errorf("The trusted checkout is not %s; remove %s and restart: %w", p.repo, p.checkout, err)
			}
			out.pins[strings.ToLower(p.repo)] = deployment
			continue
		}
		if _, ok := config.GitHubTokenFor(p.repo); !ok {
			return preparedDeployments{}, fmt.Errorf("The trusted checkout for %s needs a GitHub token; set OCTOMUS_GITHUB_TOKEN_FILE or %s", p.repo, config.RepoGitHubTokenEnv(p.repo))
		}
		fmt.Fprintf(stderr, "Cloning %s into the trusted checkout; later starts reuse it.\n", p.repo)
		partial := p.checkout + ".partial"
		_ = os.RemoveAll(partial)
		if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
			return preparedDeployments{}, err
		}
		if err := gitops.CloneRemote(ctx, p.repo, "https://github.com/"+p.repo+".git", partial, 3600); err != nil {
			_ = os.RemoveAll(partial)
			return preparedDeployments{}, fmt.Errorf("Cloning %s: %w", p.repo, err)
		}
		if err := os.Rename(partial, p.checkout); err != nil {
			return preparedDeployments{}, err
		}
		if err := checkoutValid(ctx, p.checkout, p.repo); err != nil {
			return preparedDeployments{}, fmt.Errorf("The new checkout does not verify: %w", err)
		}
		out.pins[strings.ToLower(p.repo)] = deployment
	}
	return out, nil
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
