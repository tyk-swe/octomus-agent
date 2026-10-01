package egress

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Summary is what one sandbox did through the gateway, kept until the broker collects it. Denied holds policy
// refusals; Failed holds allowlisted hosts the gateway could not reach (DNS, upstream, or the tunnel bound).
// GatewayStarted is when this gateway began counting: it never saw what a sandbox leased before then did.
type Summary struct {
	Allowed        map[string]HostCount `json:"allowed"`
	Denied         map[string]HostCount `json:"denied"`
	Failed         map[string]HostCount `json:"failed"`
	GatewayStarted time.Time            `json:"gateway_started"`
}

type HostCount struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes,omitempty"`
}

func emptySummary() Summary {
	return Summary{Allowed: map[string]HostCount{}, Denied: map[string]HostCount{}, Failed: map[string]HostCount{}}
}

// Collect returns and forgets what a finished sandbox did. A sandbox it has no entry for made no connection since
// the gateway started.
func (g *Gateway) Collect(sandboxName string) Summary {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	entry := g.stats[sandboxName]
	delete(g.stats, sandboxName)
	g.collected[sandboxName] = g.now()
	summary := emptySummary()
	if entry != nil {
		summary = entry.summary
	}
	summary.GatewayStarted = g.started
	return summary
}

// SummaryPath is where the collector answers for one sandbox, named by its "sandbox" query parameter.
const SummaryPath = "/v1/summary"

// ServeCollector answers the broker's request for a finished sandbox's summary on a local socket.
func (g *Gateway) ServeCollector(ctx context.Context, listener net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+SummaryPath, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("sandbox")
		if name == "" || len(name) > 128 {
			http.Error(w, "sandbox required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.Collect(name))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// FetchSummary collects a finished sandbox's summary from the collector on socket.
func FetchSummary(ctx context.Context, socket, sandboxName string) (Summary, error) {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://egress"+SummaryPath+"?sandbox="+url.QueryEscape(sandboxName), nil)
	if err != nil {
		return Summary{}, err
	}
	resp, err := client.Do(request)
	if err != nil {
		return Summary{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Summary{}, errors.New(resp.Status)
	}
	var summary Summary
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&summary); err != nil {
		return Summary{}, err
	}
	return summary, nil
}
