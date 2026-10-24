package chat

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Configure registers the chat alert channels as outbox destinations
// before recovery can create new attention episodes.
func Configure(st *store.Store, cfg Config) error {
	return st.SetChatDestinations(cfg.destinations())
}

// Runner owns the live adapters and workers. Chat failures warn and
// reconnect; they never stop the fleet.
type Runner struct {
	svc     *Service
	slack   *slackAdapter
	discord *discordAdapter
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// Start launches the enabled providers. It returns (nil, nil) when chat is
// disabled.
func Start(parent context.Context, cfg Config, fleet Fleet, st *store.Store, warnings io.Writer) (*Runner, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if err := Configure(st, cfg); err != nil {
		return nil, err
	}
	svc := NewService(cfg, fleet, st, warnings)
	r := &Runner{svc: svc}
	if cfg.Slack != nil {
		r.slack = newSlackAdapter(svc, cfg.Slack)
	}
	if cfg.Discord != nil {
		r.discord = newDiscordAdapter(svc, cfg.Discord)
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	spawn := func(fn func(context.Context)) {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			fn(ctx)
		}()
	}
	if r.slack != nil {
		spawn(r.slack.run)
	}
	if r.discord != nil {
		spawn(r.discord.run)
	}
	for _, pc := range []*ProviderConfig{cfg.Slack, cfg.Discord} {
		if pc == nil || pc.AlertChannel == "" {
			continue
		}
		worker := &notifyWorker{
			svc: svc, st: st, provider: pc.Provider,
			dest: store.ChatDestination{ID: DestinationID(pc.Provider, pc.AlertChannel), Kind: string(pc.Provider), Target: pc.AlertChannel},
			post: r.post,
		}
		spawn(worker.run)
	}
	if cfg.DigestTime != "" {
		spawn(func(ctx context.Context) { svc.runDigest(ctx, r.post) })
	}
	return r, nil
}

func (r *Runner) Stop() {
	r.cancel()
	r.wg.Wait()
	// Adapter loops have ended, so no new handlers can start; join the
	// ones still in flight before the caller closes the store.
	r.svc.waitInflight()
}

// post delivers one proactive message through the provider's REST API.
// The caller's context governs it, so Runner.Stop can interrupt an
// unresponsive provider instead of waiting out each HTTP timeout.
func (r *Runner) post(ctx context.Context, provider Provider, channel string, resp Response) error {
	switch provider {
	case SlackProvider:
		if r.slack == nil {
			return fmt.Errorf("slack is not configured")
		}
		return r.slack.postMessage(ctx, channel, "", resp)
	case DiscordProvider:
		if r.discord == nil {
			return fmt.Errorf("discord is not configured")
		}
		return r.discord.postMessage(ctx, channel, resp)
	}
	return fmt.Errorf("unknown chat provider %q", provider)
}
