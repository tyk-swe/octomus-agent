package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// slackAdapter speaks Socket Mode: an outbound WebSocket that carries
// slash commands, button clicks and app_mention events, so Octomus needs
// no public request URL. Replies go through the Web API / response URLs.
type slackAdapter struct {
	svc     *Service
	cfg     *ProviderConfig
	client  *http.Client
	apiBase string
}

func newSlackAdapter(svc *Service, cfg *ProviderConfig) *slackAdapter {
	return &slackAdapter{svc: svc, cfg: cfg, client: apiClient(), apiBase: "https://slack.com/api"}
}

func apiClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: 20 * time.Second}
}

type apiError struct {
	Status int
	// Code is Slack's own error string, returned with HTTP 200 in an
	// {"ok":false} body. It classifies separately from the HTTP status:
	// a transient code must keep its retries despite the 200.
	Code string
	// RetryAfter preserves the provider's own rate-limit delay (Slack
	// and Discord Retry-After headers, or Discord's JSON retry_after).
	RetryAfter time.Duration
	Msg        string
}

func (e *apiError) Error() string { return e.Msg }

// transportMessage renders an HTTP transport failure without the request
// URL. http.Client errors embed the full URL, and chat URLs carry
// per-interaction credentials (Slack response URLs, Discord interaction
// tokens) that environment-based redaction cannot know about. Only the
// wrapped cause (dial/TLS/timeout detail) survives.
func transportMessage(prefix string, err error) string {
	var urlErr *url.Error
	unwrapped := false
	for errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
		unwrapped = true
	}
	if !unwrapped || errors.As(err, &urlErr) {
		return prefix
	}
	return prefix + ": " + err.Error()
}

// retryAfterDelay parses an HTTP Retry-After header in delta-seconds
// (including Discord's fractional form) or HTTP-date form.
func retryAfterDelay(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

func (e *apiError) rateLimited() bool {
	return e.Status == http.StatusTooManyRequests || e.Code == "ratelimited" || e.Code == "rate_limited"
}

func (e *apiError) retryable() bool {
	switch e.Code {
	case "internal_error", "fatal_error", "ratelimited", "rate_limited",
		"service_unavailable", "request_timeout":
		return true
	}
	if e.Code != "" {
		return false
	}
	return e.Status == 408 || e.Status == 429 || e.Status >= 500
}

func (a *slackAdapter) run(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		started := time.Now()
		err := a.serveOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.svc.warn(fmt.Errorf("slack socket mode: %w", err))
		}
		attempt = reconnectAttempt(attempt, time.Since(started))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffDelay(attempt)):
		}
	}
}

func (a *slackAdapter) openConnection(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBase+"/apps.connections.open", strings.NewReader(""))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AppToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", &apiError{Status: 0, Msg: transportMessage("slack request failed", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	var out struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("apps.connections.open: %w", err)
	}
	if !out.OK || out.URL == "" {
		return "", &apiError{Status: resp.StatusCode, Code: out.Error, Msg: "apps.connections.open refused: " + out.Error}
	}
	return out.URL, nil
}

type slackEnvelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}

func (a *slackAdapter) serveOnce(ctx context.Context) error {
	wsURL, err := a.openConnection(ctx)
	if err != nil {
		return err
	}
	conn, err := wsDial(ctx, wsURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	for {
		data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var env slackEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch env.Type {
		case "hello":
		case "disconnect":
			return errors.New("slack asked for a reconnect")
		case "slash_commands", "interactive", "events_api":
			if err := a.handleEnvelope(ctx, conn, env); err != nil {
				return err
			}
		default:
			if env.EnvelopeID != "" {
				_ = conn.WriteJSON(map[string]any{"envelope_id": env.EnvelopeID})
			}
		}
	}
}

// handleEnvelope acknowledges within Slack's deadline first, then runs the
// (possibly slow) engine call in the background and posts the real answer.
func (a *slackAdapter) handleEnvelope(ctx context.Context, conn *wsConn, env slackEnvelope) error {
	inv, ackPayload, respond, ok := a.decode(env)
	authorized := ok && a.svc != nil && a.svc.Authorized(inv.Provider, inv.UserID)
	if ok && !authorized {
		// The envelope still needs its acknowledgement, but a
		// non-allowlisted sender must receive no visible response
		// payload (not even "Working on it…").
		ackPayload = nil
	}
	ack := map[string]any{"envelope_id": env.EnvelopeID}
	if ackPayload != nil {
		ack["payload"] = ackPayload
	}
	if err := conn.WriteJSON(ack); err != nil {
		return err
	}
	if !ok || !authorized {
		return nil
	}
	a.svc.goHandle(func() {
		resp, handled := a.svc.Handle(ctx, inv)
		if !handled || (resp.Text == "" && len(resp.Buttons) == 0) {
			return
		}
		if err := respond(resp); err != nil {
			a.svc.warn(err)
		}
	})
	return nil
}

type slackSlashPayload struct {
	Command     string `json:"command"`
	Text        string `json:"text"`
	UserID      string `json:"user_id"`
	ChannelID   string `json:"channel_id"`
	ResponseURL string `json:"response_url"`
}

type slackInteractivePayload struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		Value string `json:"value"`
	} `json:"actions"`
}

type slackEventPayload struct {
	EventID string `json:"event_id"`
	Event   struct {
		Type     string `json:"type"`
		User     string `json:"user"`
		Channel  string `json:"channel"`
		Text     string `json:"text"`
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"event"`
}

func (a *slackAdapter) decode(env slackEnvelope) (Invocation, map[string]any, func(Response) error, bool) {
	switch env.Type {
	case "slash_commands":
		var p slackSlashPayload
		if json.Unmarshal(env.Payload, &p) != nil || p.Command != "/octomus" {
			return Invocation{}, nil, nil, false
		}
		inv := Invocation{Provider: SlackProvider, EventID: env.EnvelopeID, UserID: p.UserID, ChannelID: p.ChannelID, Text: p.Text}
		ack := map[string]any{"response_type": "ephemeral", "text": "Working on it…"}
		respond := func(resp Response) error {
			return a.postResponseURL(p.ResponseURL, resp, true)
		}
		return inv, ack, respond, true
	case "interactive":
		var p slackInteractivePayload
		if json.Unmarshal(env.Payload, &p) != nil || len(p.Actions) == 0 {
			return Invocation{}, nil, nil, false
		}
		inv := Invocation{Provider: SlackProvider, EventID: env.EnvelopeID, UserID: p.User.ID,
			ChannelID: p.Channel.ID, ThreadID: p.Message.ThreadTS, Action: p.Actions[0].Value}
		respond := func(resp Response) error {
			return a.postResponseURL(p.ResponseURL, resp, false)
		}
		return inv, nil, respond, true
	case "events_api":
		var p slackEventPayload
		if json.Unmarshal(env.Payload, &p) != nil || p.Event.Type != "app_mention" {
			return Invocation{}, nil, nil, false
		}
		thread := p.Event.ThreadTS
		if thread == "" {
			thread = p.Event.TS
		}
		inv := Invocation{Provider: SlackProvider, EventID: p.EventID, UserID: p.Event.User,
			ChannelID: p.Event.Channel, ThreadID: thread, Text: stripMention(p.Event.Text)}
		respond := func(resp Response) error {
			return a.postMessage(context.Background(), p.Event.Channel, thread, resp)
		}
		return inv, nil, respond, true
	}
	return Invocation{}, nil, nil, false
}

func stripMention(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<@") {
		if end := strings.Index(text, ">"); end >= 0 {
			text = strings.TrimSpace(text[end+1:])
		}
	}
	return text
}

// slackBlocks renders text plus buttons as Block Kit, keeping text as the
// notification fallback.
func slackBlocks(resp Response) []map[string]any {
	blocks := []map[string]any{
		// verbatim stops Slack parsing @mentions, #channels and links out
		// of repository-derived text (proposal and PR titles, task errors).
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": slackEscapedBounded(resp.Text), "verbatim": true}},
	}
	if len(resp.Buttons) > 0 {
		elements := []map[string]any{}
		for i, b := range resp.Buttons {
			if len(elements) >= 5 {
				break
			}
			// Slack requires action IDs unique within the block; clicks
			// dispatch on value, so the ID only needs to be distinct.
			elements = append(elements, map[string]any{
				"type": "button", "action_id": fmt.Sprintf("octomus_action_%d", i), "value": b.Value,
				"text": map[string]any{"type": "plain_text", "text": b.Label},
			})
		}
		blocks = append(blocks, map[string]any{"type": "actions", "elements": elements})
	}
	return blocks
}

func slackEscape(text string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(text)
}

// slackEscapedBounded escapes text and keeps the result within Slack's
// 3000-character section limit: escaping expands text (& → &amp;), so the
// bound must apply after escaping, never splitting an entity.
func slackEscapedBounded(text string) string {
	escaped := slackEscape(text)
	runes := []rune(escaped)
	const sectionLimit = 3000
	if len(runes) <= sectionLimit {
		return escaped
	}
	cut := string(runes[:sectionLimit-1])
	if amp, semi := strings.LastIndex(cut, "&"), strings.LastIndex(cut, ";"); amp > semi {
		cut = cut[:amp]
	}
	return cut + "…"
}

func (a *slackAdapter) postResponseURL(responseURL string, resp Response, replace bool) error {
	if responseURL == "" {
		return nil
	}
	body := map[string]any{
		"response_type": "ephemeral", "replace_original": replace,
		// The fallback text is markup-parsed by older clients, so it gets
		// the same escaping as the block text.
		"text": slackEscape(resp.Text), "blocks": slackBlocks(resp),
	}
	return a.postJSON(context.Background(), responseURL, "", body)
}

// postMessage sends a proactive message (alerts, digest, mention replies).
func (a *slackAdapter) postMessage(ctx context.Context, channel, thread string, resp Response) error {
	// mrkdwn:false keeps the fallback text literal; blocks carry the
	// rendered form with mention parsing disabled there too.
	body := map[string]any{"channel": channel, "text": resp.Text, "mrkdwn": false, "blocks": slackBlocks(resp)}
	if thread != "" {
		body["thread_ts"] = thread
	}
	return a.postJSON(ctx, a.apiBase+"/chat.postMessage", a.cfg.BotToken, body)
}

func (a *slackAdapter) postJSON(ctx context.Context, rawURL, token string, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(data))
	if err != nil {
		return &apiError{Status: 0, Msg: transportMessage("slack request failed", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return &apiError{Status: 0, Msg: transportMessage("slack request failed", err)}
	}
	defer resp.Body.Close()
	retryAfter := retryAfterDelay(resp)
	got, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return &apiError{Status: 0, Msg: transportMessage("slack response unreadable", err)}
	}
	var out struct {
		OK         *bool   `json:"ok"`
		Error      string  `json:"error"`
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(got, &out) == nil && out.RetryAfter > 0 {
		retryAfter = max(retryAfter, time.Duration(out.RetryAfter*float64(time.Second)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{Status: resp.StatusCode, RetryAfter: retryAfter, Msg: fmt.Sprintf("slack request refused: %s", resp.Status)}
	}
	if json.Unmarshal(got, &out) == nil && out.OK != nil {
		if !*out.OK {
			if out.RetryAfter > 0 {
				retryAfter = max(retryAfter, time.Duration(out.RetryAfter*float64(time.Second)))
			}
			return &apiError{Status: resp.StatusCode, Code: out.Error, RetryAfter: retryAfter, Msg: "slack refused: " + out.Error}
		}
		return nil
	}
	if token != "" {
		// Web API calls must positively acknowledge with ok:true; an
		// undecodable 200 is not a delivery. Status 0 keeps it on the
		// notification worker's retryable transport path.
		return &apiError{Status: 0, Msg: "slack response unreadable"}
	}
	// Response URLs acknowledge in plain text ("ok"): a 2xx suffices.
	return nil
}
