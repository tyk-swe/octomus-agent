package egress

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Summary is what one sandbox did through the gateway, kept until the broker collects it. Denied holds policy
// refusals; Failed holds allowlisted hosts the gateway could not reach (DNS, upstream, or the tunnel bound).
// GatewayStarted is when this gateway began counting: it never saw what a sandbox leased before then did.
// Incomplete means at least one request had no recorded outcome when collected.
type Summary struct {
	Allowed        map[string]HostCount `json:"allowed"`
	Denied         map[string]HostCount `json:"denied"`
	Failed         map[string]HostCount `json:"failed"`
	GatewayStarted time.Time            `json:"gateway_started"`
	Incomplete     bool                 `json:"incomplete,omitempty"`
}

type HostCount struct {
	Count int `json:"count"`
}

func emptySummary() Summary {
	return Summary{Allowed: map[string]HostCount{}, Denied: map[string]HostCount{}, Failed: map[string]HostCount{}}
}

// summaryPath is where the collector answers for one sandbox, named by its "sandbox" query parameter.
const summaryPath = "/v1/summary"

// probeTargetPath answers with one DNS target outside the gateway's effective runner allowlist. It exposes no
// credentials or full policy, and is served only on the broker's local collector socket.
const probeTargetPath = "/v1/probe-target"

// ServeCollector answers the broker's request for a finished sandbox's summary on a local socket.
func (g *Gateway) ServeCollector(ctx context.Context, listener net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+probeTargetPath, func(w http.ResponseWriter, r *http.Request) {
		target, err := g.policy.probeTarget()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(target)
	})
	mux.HandleFunc("GET "+summaryPath, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("sandbox")
		if name == "" || len(name) > 128 {
			http.Error(w, "sandbox required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.collect(name))
	})
	return serveUntil(ctx, &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}, listener)
}

// fetch asks the collector on socket for path and decodes its JSON answer, of at most limit bytes, into out.
func fetch(ctx context.Context, socket, path string, limit int64, out any) error {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://egress"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(out)
}

// FetchProbeTarget reads the target from the running gateway, never from a copy of deployment configuration that
// may differ from its policy. An unavailable or malformed answer cannot prove an allowlist refusal.
func FetchProbeTarget(ctx context.Context, socket string) (string, error) {
	var target string
	if err := fetch(ctx, socket, probeTargetPath, 1024, &target); err != nil {
		return "", err
	}
	if !wire.ValidProbeTarget(target) {
		return "", errors.New("invalid containment probe target")
	}
	return target, nil
}

// FetchSummary collects a finished sandbox's summary from the collector on socket.
func FetchSummary(ctx context.Context, socket, sandboxName string) (Summary, error) {
	var summary Summary
	if err := fetch(ctx, socket, summaryPath+"?sandbox="+url.QueryEscape(sandboxName), 1<<20, &summary); err != nil {
		return Summary{}, err
	}
	return summary, nil
}
