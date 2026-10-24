package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// discordAdapter speaks the Discord Gateway (outbound WebSocket) for
// application commands and message components, plus REST for replies,
// command registration and proactive messages. No public endpoint needed.
type discordAdapter struct {
	svc     *Service
	cfg     *ProviderConfig
	client  *http.Client
	apiBase string

	mu        sync.Mutex
	sessionID string
	resumeURL string
	seq       *int64
	hbAcked   bool // the last heartbeat was acknowledged (opcode 11)
}

func newDiscordAdapter(svc *Service, cfg *ProviderConfig) *discordAdapter {
	return &discordAdapter{svc: svc, cfg: cfg, client: apiClient(), apiBase: "https://discord.com/api/v10"}
}

func (a *discordAdapter) run(ctx context.Context) {
	// Registration retries on its own loop: a transient failure at first
	// startup must not leave /octomus unregistered until a restart, and
	// must not block the gateway either.
	a.svc.goHandle(func() { a.registerLoop(ctx) })
	attempt := 0
	for ctx.Err() == nil {
		err := a.serveOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.svc.warn(fmt.Errorf("discord gateway: %w", err))
		}
		attempt++
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffDelay(attempt)):
		}
	}
}

func (a *discordAdapter) gatewayURL(ctx context.Context) (string, error) {
	a.mu.Lock()
	resume := a.resumeURL
	a.mu.Unlock()
	if resume != "" {
		return resume, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.apiBase+"/gateway/bot", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+a.cfg.BotToken)
	resp, err := a.client.Do(req)
	if err != nil {
		return "", &apiError{Status: 0, Msg: transportMessage("discord request failed", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	var out struct {
		URL string `json:"url"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(body, &out) != nil || out.URL == "" {
		return "", &apiError{Status: resp.StatusCode, Msg: "gateway/bot refused: " + resp.Status}
	}
	return out.URL, nil
}

type discordPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

func (a *discordAdapter) serveOnce(ctx context.Context) error {
	gw, err := a.gatewayURL(ctx)
	if err != nil {
		return err
	}
	if !strings.Contains(gw, "?") {
		gw += "?v=10&encoding=json"
	}
	conn, err := wsDial(ctx, gw)
	if err != nil {
		return err
	}
	defer conn.Close()
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	for {
		data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var closeErr *wsCloseError
			if errors.As(err, &closeErr) && !discordResumable(closeErr.Code) {
				// The gateway discarded this session (e.g. 4007 invalid
				// sequence, 4009 session timed out): forget it so the
				// next connection Identifies fresh instead of Resuming
				// a rejected session forever.
				a.clearSession()
			}
			return err
		}
		var p discordPayload
		if json.Unmarshal(data, &p) != nil {
			continue
		}
		if p.S != nil {
			a.mu.Lock()
			a.seq = p.S
			a.mu.Unlock()
		}
		switch p.Op {
		case 1: // Heartbeat request: answer immediately, timer untouched.
			a.mu.Lock()
			seq := a.seq
			a.hbAcked = false
			a.mu.Unlock()
			if err := conn.WriteJSON(map[string]any{"op": 1, "d": seq}); err != nil {
				return err
			}
		case 11: // Heartbeat ACK.
			a.mu.Lock()
			a.hbAcked = true
			a.mu.Unlock()
		case 10: // Hello: start heartbeats, then identify or resume.
			var hello struct {
				HeartbeatInterval int64 `json:"heartbeat_interval"`
			}
			_ = json.Unmarshal(p.D, &hello)
			a.mu.Lock()
			a.hbAcked = true
			a.mu.Unlock()
			go a.heartbeat(connCtx, conn, time.Duration(hello.HeartbeatInterval)*time.Millisecond)
			if err := a.identify(conn); err != nil {
				return err
			}
		case 0: // Dispatch.
			switch p.T {
			case "READY":
				var ready struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
				}
				if json.Unmarshal(p.D, &ready) == nil {
					a.mu.Lock()
					a.sessionID, a.resumeURL = ready.SessionID, ready.ResumeGatewayURL
					a.mu.Unlock()
				}
			case "INTERACTION_CREATE":
				// The REST acknowledgement can take seconds; the read
				// loop must stay free for heartbeats and the next
				// interaction's own acknowledgement deadline. The work
				// runs on the adapter-lifetime context, not connCtx: a
				// gateway reconnect must not cancel an acknowledgement
				// Discord will never replay.
				a.svc.goHandle(func() { a.handleInteraction(ctx, p.D) })
			}
		case 7: // Reconnect.
			return errors.New("discord asked for a reconnect")
		case 9: // Invalid session: forget it and re-identify fresh.
			a.clearSession()
			return errors.New("discord session invalid; re-identifying")
		}
	}
}

func (a *discordAdapter) clearSession() {
	a.mu.Lock()
	a.sessionID, a.resumeURL, a.seq = "", "", nil
	a.mu.Unlock()
}

// discordResumable reports whether a gateway close code permits Resume
// (Discord's close-code table: 4000–4003, 4005, 4008 may reconnect and
// resume; 4004, 4007, 4009 and the 401x configuration codes may not).
func discordResumable(code uint16) bool {
	switch code {
	case 4004, 4007, 4009, 4010, 4011, 4012, 4013, 4014:
		return false
	}
	return true
}

func (a *discordAdapter) identify(conn *wsConn) error {
	a.mu.Lock()
	session, seq := a.sessionID, a.seq
	a.mu.Unlock()
	if session != "" {
		return conn.WriteJSON(map[string]any{"op": 6, "d": map[string]any{
			"token": a.cfg.BotToken, "session_id": session, "seq": seq,
		}})
	}
	// Interactions arrive without privileged intents; GUILDS (1<<0) is all
	// this adapter requests. No message-content access is used.
	return conn.WriteJSON(map[string]any{"op": 2, "d": map[string]any{
		"token":   a.cfg.BotToken,
		"intents": 1,
		"properties": map[string]any{
			"os": "linux", "browser": "octomus-agent", "device": "octomus-agent",
		},
	}})
}

func (a *discordAdapter) heartbeat(ctx context.Context, conn *wsConn, interval time.Duration) {
	if interval <= 0 {
		interval = 45 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.mu.Lock()
			seq, acked := a.seq, a.hbAcked
			a.hbAcked = false
			a.mu.Unlock()
			if !acked {
				// Half-open connection: writes may keep succeeding
				// while nothing arrives. Closing forces the read
				// loop out so run() reconnects.
				conn.Close()
				return
			}
			if err := conn.WriteJSON(map[string]any{"op": 1, "d": seq}); err != nil {
				conn.Close()
				return
			}
		}
	}
}

type discordInteraction struct {
	ID        string `json:"id"`
	Type      int    `json:"type"`
	Token     string `json:"token"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Member    *struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	} `json:"member"`
	User *struct {
		ID string `json:"id"`
	} `json:"user"`
	Data *struct {
		Name     string `json:"name"`
		CustomID string `json:"custom_id"`
		Options  []struct {
			Name    string `json:"name"`
			Value   any    `json:"value"`
			Options []struct {
				Name  string `json:"name"`
				Value any    `json:"value"`
			} `json:"options"`
		} `json:"options"`
	} `json:"data"`
}

func (it *discordInteraction) userID() string {
	if it.Member != nil {
		return it.Member.User.ID
	}
	if it.User != nil {
		return it.User.ID
	}
	return ""
}

// commandText flattens a /octomus subcommand invocation into the shared
// text command language the router speaks.
func (it *discordInteraction) commandText() (string, bool) {
	if it.Data == nil || it.Data.Name != "octomus" || len(it.Data.Options) == 0 {
		return "", false
	}
	sub := it.Data.Options[0]
	parts := []string{sub.Name}
	for _, opt := range sub.Options {
		value := fmt.Sprint(opt.Value)
		if opt.Name == "repo" {
			parts = append(parts, "--repo", value)
		} else {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " "), true
}

// handleInteraction defers within Discord's deadline, then edits the
// deferred reply with the engine-backed answer.
func (a *discordAdapter) handleInteraction(ctx context.Context, raw json.RawMessage) {
	var it discordInteraction
	if json.Unmarshal(raw, &it) != nil {
		return
	}
	var inv Invocation
	callbackType := 5 // deferred channel message (ephemeral via flags)
	switch it.Type {
	case 2: // application command
		text, ok := it.commandText()
		if !ok {
			return
		}
		inv = Invocation{Provider: DiscordProvider, EventID: it.ID, UserID: it.userID(), ChannelID: it.ChannelID, Text: text}
	case 3: // message component
		if it.Data == nil {
			return
		}
		inv = Invocation{Provider: DiscordProvider, EventID: it.ID, UserID: it.userID(), ChannelID: it.ChannelID, Action: it.Data.CustomID}
		callbackType = 6 // deferred update of the clicked message
	default:
		return
	}
	if !a.svc.Authorized(DiscordProvider, inv.UserID) {
		// Never answer a stranger with a deferred update (type 6): the
		// follow-up edit would replace the shared alert message, letting
		// any channel member erase it. An ephemeral reply edits nothing.
		refusal := map[string]any{"type": 4, "data": map[string]any{
			"content": "Octomus chat is operator-only.", "flags": 64,
			"allowed_mentions": noMentions(),
		}}
		if err := a.rest(ctx, http.MethodPost, fmt.Sprintf("/interactions/%s/%s/callback", it.ID, it.Token), refusal, false); err != nil {
			a.svc.warn(err)
		}
		return
	}
	callback := map[string]any{"type": callbackType}
	if callbackType == 5 {
		callback["data"] = map[string]any{"flags": 64}
	}
	if err := a.rest(ctx, http.MethodPost, fmt.Sprintf("/interactions/%s/%s/callback", it.ID, it.Token), callback, false); err != nil {
		a.svc.warn(err)
		return
	}
	a.svc.goHandle(func() {
		resp, handled := a.svc.Handle(ctx, inv)
		if !handled {
			// Authorization was checked before the callback, so this is
			// only a config change mid-flight. For a component click the
			// original message is the shared alert: never edit it here.
			if callbackType == 6 {
				return
			}
			resp = Response{Text: "Octomus chat is operator-only."}
		}
		if resp.Text == "" && len(resp.Buttons) == 0 {
			resp.Text = "Done."
		}
		body := map[string]any{"content": resp.Text, "components": discordComponents(resp), "allowed_mentions": noMentions()}
		path := fmt.Sprintf("/webhooks/%s/%s/messages/@original", a.cfg.AppID, it.Token)
		if err := a.rest(context.Background(), http.MethodPatch, path, body, false); err != nil {
			a.svc.warn(err)
		}
	})
}

// noMentions disables mention parsing: response text carries
// model-generated titles and error strings, which must display as text,
// never ping users, roles or @everyone.
func noMentions() map[string]any {
	return map[string]any{"parse": []string{}}
}

func discordComponents(resp Response) []map[string]any {
	if len(resp.Buttons) == 0 {
		return []map[string]any{}
	}
	buttons := []map[string]any{}
	for _, b := range resp.Buttons {
		if len(buttons) >= 5 {
			break
		}
		if len(b.Value) > 100 {
			// Discord rejects the whole message for one oversize
			// custom_id; dropping the button keeps the alert itself.
			continue
		}
		style := 2
		if strings.HasPrefix(b.Value, "confirm:") {
			style = 1
		}
		buttons = append(buttons, map[string]any{
			"type": 2, "style": style, "label": b.Label, "custom_id": b.Value,
		})
	}
	if len(buttons) == 0 {
		return []map[string]any{}
	}
	return []map[string]any{{"type": 1, "components": buttons}}
}

// postMessage sends a proactive message (alerts, digest) to a channel.
func (a *discordAdapter) postMessage(ctx context.Context, channel string, resp Response) error {
	body := map[string]any{"content": resp.Text, "components": discordComponents(resp), "allowed_mentions": noMentions()}
	return a.rest(ctx, http.MethodPost, "/channels/"+channel+"/messages", body, true)
}

func (a *discordAdapter) rest(ctx context.Context, method, path string, body map[string]any, bot bool) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, a.apiBase+path, bytes.NewReader(data))
	if err != nil {
		return &apiError{Status: 0, Msg: transportMessage("discord request failed", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	if bot {
		req.Header.Set("Authorization", "Bot "+a.cfg.BotToken)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return &apiError{Status: 0, Msg: transportMessage("discord request failed", err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{Status: resp.StatusCode, Msg: fmt.Sprintf("discord request refused: %s", resp.Status)}
	}
	return nil
}

// registerLoop registers the /octomus command, retrying transient
// failures (429/5xx/network) with backoff until it succeeds or ctx ends.
// Permanent refusals warn once and stop.
func (a *discordAdapter) registerLoop(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		err := a.registerCommands(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		a.svc.warn(fmt.Errorf("discord command registration: %w", err))
		var apiErr *apiError
		if errors.As(err, &apiErr) && !apiErr.retryable() && apiErr.Status != 0 {
			return
		}
		attempt++
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffDelay(attempt)):
		}
	}
}

// registerCommands bulk-overwrites the /octomus command. Guild commands
// propagate instantly; without a guild it registers globally (slow).
func (a *discordAdapter) registerCommands(ctx context.Context) error {
	strOpt := func(name, desc string, required bool) map[string]any {
		return map[string]any{"type": 3, "name": name, "description": desc, "required": required}
	}
	repoOpt := strOpt("repo", "OWNER/REPOSITORY (default: bound or primary repository)", false)
	sub := func(name, desc string, opts ...map[string]any) map[string]any {
		o := append([]map[string]any{}, opts...)
		o = append(o, repoOpt)
		return map[string]any{"type": 1, "name": name, "description": desc, "options": o}
	}
	command := map[string]any{
		"name": "octomus", "description": "Octomus repository maintainer", "type": 1,
		"options": []map[string]any{
			sub("status", "Mode, cycle, active tasks, today's sessions"),
			sub("digest", "7-day summary for every repository"),
			sub("tasks", "Latest tasks", strOpt("count", "How many (default 5)", false)),
			sub("task", "One task's detail (default: the remembered task)", strOpt("id", "Task ID (default: the remembered task)", false)),
			sub("cycles", "Latest cycles", strOpt("count", "How many (default 5)", false)),
			sub("prs", "Observed pull requests", strOpt("count", "How many (default 5)", false)),
			sub("requests", "Pending work requests"),
			sub("request", "File work for the next planning cycle", strOpt("text", "Title :: description", true)),
			sub("pause", "Pause this repository"),
			sub("resume", "Resume continuous operation"),
			sub("cycle", "Run one cycle"),
			sub("audit", "Run an audit"),
			sub("retry", "Retry a task", strOpt("id", "Task ID", true)),
			sub("cancel", "Cancel a task", strOpt("id", "Task ID", true)),
			sub("withdraw", "Withdraw a work request", strOpt("id", "Request ID", true)),
			sub("repos", "Repositories this instance serves"),
			sub("help", "Show command help"),
		},
	}
	path := "/applications/" + a.cfg.AppID + "/commands"
	if a.cfg.GuildID != "" {
		path = "/applications/" + a.cfg.AppID + "/guilds/" + a.cfg.GuildID + "/commands"
	}
	data, err := json.Marshal([]map[string]any{command})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.apiBase+path, bytes.NewReader(data))
	if err != nil {
		return &apiError{Status: 0, Msg: transportMessage("discord request failed", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+a.cfg.BotToken)
	resp, err := a.client.Do(req)
	if err != nil {
		return &apiError{Status: 0, Msg: transportMessage("discord request failed", err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{Status: resp.StatusCode, Msg: "command registration refused: " + resp.Status}
	}
	return nil
}
