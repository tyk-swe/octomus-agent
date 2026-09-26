package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	octomus "github.com/tyk-swe/octomus-agent"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const CodexTestedVersion = "0.153.4"

func CodexVersionWarning(installed string) *string {
	expected := "codex-cli " + CodexTestedVersion
	if strings.TrimSpace(installed) == expected {
		return nil
	}
	warning := VersionWarning(config.BackendCodex, installed,
		fmt.Sprintf("tested %s. Pin the tested CLI before live commissioning", expected))
	return &warning
}

var errCodexDisconnected = errors.New("Codex app-server disconnected")

type queuedMessage struct {
	value map[string]any
	size  int
}

// backlog keeps, in order, the notifications that arrive before an RPC
// response, bounded to 10000 messages and 8 MiB of compact JSON.
type backlog struct {
	items []queuedMessage
	bytes int
}

func (b *backlog) push(value map[string]any) error {
	if len(b.items) >= 10000 {
		return fmt.Errorf("Codex notification backlog exceeded")
	}
	encoded, err := marshal(value)
	if err != nil {
		return err
	}
	if b.bytes+len(encoded) > 8*1024*1024 {
		return fmt.Errorf("Codex notification backlog exceeds 8 MB")
	}
	b.bytes += len(encoded)
	b.items = append(b.items, queuedMessage{value, len(encoded)})
	return nil
}

// pop removes and returns the oldest queued notification.
func (b *backlog) pop() (map[string]any, bool) {
	if len(b.items) == 0 {
		return nil, false
	}
	queued := b.items[0]
	b.items[0] = queuedMessage{}
	b.items = b.items[1:]
	if len(b.items) == 0 {
		b.items = nil
	}
	b.bytes -= queued.size
	return queued.value, true
}

// Codex owns a `codex app-server --listen stdio://` child and speaks its
// newline-delimited JSON-RPC protocol.
type Codex struct {
	child    *process.GroupChild
	stdin    *os.File
	stdout   *os.File
	lines    chan lineResult
	serial   uint64
	pending  backlog
	timeout  uint64
	ctx      context.Context
	state    *store.Store
	entity   string
	waitCh   chan error
	done     chan struct{}
	once     sync.Once
	closeErr error
	// binary and commandTimeout bound the Diagnostics version check.
	binary         string
	commandTimeout uint64
}

func ConnectCodex(ctx context.Context, cfg config.Config, cwd string, state *store.Store, entity string) (*Codex, error) {
	if ctx.Err() != nil {
		return nil, process.ErrSessionCancelled
	}
	cmd := process.Command(cfg.CodexBinary, cwd)
	cmd.Args = append(cmd.Args, "app-server", "--listen", "stdio://")
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, err
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	// Stderr is kept only as a bounded tail that explains a connect failure.
	tail := &stderrTail{}
	cmd.Stderr = tail
	cmd.WaitDelay = stderrWaitDelay
	if err := cmd.Start(); err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("Could not start Codex app-server; install and authenticate Codex on this host: %w", err)
	}
	// The child holds its own pipe ends now; the parent's copies must close or
	// the reader would never see end-of-stream.
	stdinR.Close()
	stdoutW.Close()
	done := make(chan struct{})
	c := &Codex{
		child:   process.NewGroupChild(cmd),
		stdin:   stdinW,
		stdout:  stdoutR,
		lines:   lineReader(stdoutR, MaxMessage, done),
		timeout: cfg.SessionTimeoutSeconds,
		ctx:     ctx,
		state:   state,
		entity:  entity,
		waitCh:  make(chan error, 1),
		done:    done,

		binary:         cfg.CodexBinary,
		commandTimeout: cfg.CommandTimeoutSeconds,
	}
	go func() { c.waitCh <- cmd.Wait() }()
	fail := func(err error) (*Codex, error) {
		_ = c.Close()
		return nil, tail.explain(err)
	}
	if _, err := c.rpc("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "octomus_agent", "title": "Octomus Agent", "version": octomus.Version},
		"capabilities": map[string]any{"experimentalApi": false},
	}); err != nil {
		return fail(err)
	}
	if err := c.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return fail(err)
	}
	return c, nil
}

// Diagnostics reports authentication state plus the installed CLI's version
// against the tested baseline.
func (c *Codex) Diagnostics(cwd string) (map[string]any, error) {
	account, err := c.rpc("account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return nil, err
	}
	m, _ := asObject(account)
	if m["requiresOpenaiAuth"] != false && m["account"] == nil {
		return nil, fmt.Errorf("Codex authentication is missing; run codex login as the service user")
	}
	version, err := process.RunMachine(c.ctx, c.binary, []string{"--version"}, cwd, min(c.commandTimeout, 60))
	if err != nil {
		return nil, err
	}
	version = strings.TrimSpace(version)
	return diagnosticsValue(config.BackendCodex, version, CodexTestedVersion, CodexVersionWarning(version)), nil
}

// framed marshals a protocol message and applies the exact outbound bound to
// the JSON content: MaxMessage bytes of content is accepted, one byte more is
// rejected, and the newline terminator is not counted against the bound.
func framed(value map[string]any) (string, error) {
	payload, err := marshal(value)
	if err != nil {
		return "", err
	}
	if len(payload) > MaxMessage {
		return "", errors.New("Codex message exceeds the 16 MB protocol limit")
	}
	return payload + "\n", nil
}

func (c *Codex) send(value map[string]any) error {
	payload, err := framed(value)
	if err != nil {
		return err
	}
	_, err = process.Bounded(c.ctx, min(c.timeout, 60), "Codex write timed out", func(wctx context.Context) (struct{}, error) {
		return struct{}{}, writeAll(wctx, c.stdin, []byte(payload))
	})
	return err
}

// sendBestEffort writes without checking the owner context: cleanup requests
// must still reach the server after operator cancellation. The caller's bound
// supplies the write context.
func (c *Codex) sendBestEffort(ctx context.Context, value map[string]any) error {
	payload, err := framed(value)
	if err != nil {
		return err
	}
	return writeAll(ctx, c.stdin, []byte(payload))
}

// writeAll observes ctx so a write blocked on a full pipe unwinds with the
// caller instead of stranding a goroutine. A rolling write deadline of at
// most 250ms, clipped to the context deadline, turns a blocking write into a
// retryable timeout while the context is live.
func writeAll(ctx context.Context, w *os.File, data []byte) error {
	defer w.SetWriteDeadline(time.Time{})
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		deadline := time.Now().Add(250 * time.Millisecond)
		if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
			deadline = limit
		}
		if err := w.SetWriteDeadline(deadline); err != nil {
			return err
		}
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				continue
			}
			return err
		}
	}
	return nil
}

// receive reads one protocol message by deadline, answering and rejecting
// interactive JSON-RPC requests. what names the bound in the timeout error.
func (c *Codex) receive(deadline time.Time, what string) (map[string]any, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var r lineResult
	select {
	case line, ok := <-c.lines:
		if !ok {
			return nil, errCodexDisconnected
		}
		r = line
	case <-timer.C:
		return nil, fmt.Errorf("%s: deadline has elapsed", what)
	case <-c.ctx.Done():
		return nil, process.ErrSessionCancelled
	}
	if r.err != nil {
		return nil, r.err
	}
	decoded, err := decodeJSON(r.line)
	if err != nil {
		return nil, fmt.Errorf("Invalid app-server JSON: %w", err)
	}
	v, _ := asObject(decoded)
	if v == nil {
		return map[string]any{}, nil
	}
	id, hasID := v["id"]
	if _, hasMethod := v["method"]; hasMethod && hasID {
		if err := c.send(map[string]any{
			"id":    id,
			"error": map[string]any{"code": -32000, "message": "Octomus unattended mode cannot answer interactive requests"},
		}); err != nil {
			return nil, err
		}
		name, _ := marshal(v["method"])
		return nil, fmt.Errorf("Codex requested interactive input (%s); task blocked", name)
	}
	return v, nil
}

func (c *Codex) rpc(method string, params map[string]any) (any, error) {
	c.serial++
	id := c.serial
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	// A turn can emit notifications before the request response; preserve
	// their order. The whole call is bounded at 60 s and each message by the
	// session timeout, whichever ends first.
	deadline := time.Now().Add(60 * time.Second)
	for {
		bound, what := deadline, "Codex RPC timed out"
		if perMessage := time.Now().Add(time.Duration(c.timeout) * time.Second); perMessage.Before(bound) {
			bound, what = perMessage, "Codex response timed out"
		}
		v, err := c.receive(bound, what)
		if err != nil {
			return nil, err
		}
		if idMatches(v["id"], id) {
			if e, hasErr := v["error"]; hasErr {
				encoded, _ := marshal(e)
				return nil, fmt.Errorf("Codex %s: %s", method, store.Redact(encoded))
			}
			result, ok := v["result"]
			if !ok {
				return nil, fmt.Errorf("Missing RPC result")
			}
			return result, nil
		}
		if err := c.pending.push(v); err != nil {
			return nil, err
		}
	}
}

func idMatches(v any, id uint64) bool {
	n, ok := v.(json.Number)
	return ok && n.String() == strconv.FormatUint(id, 10)
}

// Models lists the catalog with model/list pagination.
func (c *Codex) Models(cwd string) ([]Model, error) {
	out := []Model{}
	var cursor any
	for {
		v, err := c.rpc("model/list", map[string]any{"limit": 100, "cursor": cursor, "includeHidden": true})
		if err != nil {
			return nil, err
		}
		result, _ := asObject(v)
		data, ok := asArray(result["data"])
		if !ok {
			return nil, fmt.Errorf("Invalid model catalog")
		}
		for _, item := range data {
			m, _ := asObject(item)
			name, ok := strAt(m, "model")
			if !ok {
				return nil, fmt.Errorf("Invalid Codex model identity")
			}
			display, _ := strAt(m, "displayName")
			effortList, ok := asArray(m["supportedReasoningEfforts"])
			if !ok {
				return nil, fmt.Errorf("Invalid Codex reasoning catalog")
			}
			efforts := []string{}
			for _, e := range effortList {
				entry, _ := asObject(e)
				effort, ok := strAt(entry, "reasoningEffort")
				if !ok {
					return nil, fmt.Errorf("Invalid reasoning effort")
				}
				efforts = append(efforts, effort)
			}
			out = append(out, Model{
				Backend:     config.BackendCodex,
				Model:       name,
				DisplayName: display,
				Efforts:     efforts,
				Variants:    []string{},
				Available:   true,
			})
		}
		cursor = result["nextCursor"]
		if cursor == nil {
			break
		}
		// A continuing page must make progress: an empty page with a cursor,
		// or a catalog past the model bound, would page forever.
		if len(data) == 0 || len(out) >= 10000 {
			return nil, fmt.Errorf("Invalid model pagination")
		}
	}
	return out, nil
}

func (c *Codex) Start(route config.Route, cwd string, resume *string) (string, error) {
	if err := requireRoute(route, config.BackendCodex); err != nil {
		return "", err
	}
	params := map[string]any{
		"model":                 route.Model,
		"cwd":                   cwd,
		"approvalPolicy":        "never",
		"sandbox":               "danger-full-access",
		"config":                map[string]any{"model_reasoning_effort": route.Effort},
		"developerInstructions": WorkerInstructions,
	}
	method := "thread/start"
	if resume != nil {
		params["threadId"] = *resume
		method = "thread/resume"
	}
	v, err := c.rpc(method, params)
	if err != nil {
		return "", err
	}
	result, _ := asObject(v)
	if s, _ := strAt(result, "model"); s != route.Model {
		return "", fmt.Errorf("Runtime substituted the requested model")
	}
	if s, _ := strAt(result, "reasoningEffort"); s != route.Effort {
		return "", fmt.Errorf("Runtime substituted the requested reasoning effort")
	}
	sandbox, _ := asObject(result["sandbox"])
	sandboxType, _ := strAt(sandbox, "type")
	if result["approvalPolicy"] != "never" || sandboxType != "dangerFullAccess" {
		return "", fmt.Errorf("Runtime did not grant the configured no-sandbox/never-approve policy")
	}
	thread, _ := asObject(result["thread"])
	identity, ok := strAt(thread, "id")
	if !ok {
		return "", fmt.Errorf("Missing Codex thread identity")
	}
	if _, err := uuid.Parse(identity); err != nil {
		return "", fmt.Errorf("Invalid Codex thread identity: %w", err)
	}
	if resume != nil && identity != *resume {
		return "", fmt.Errorf("Codex substituted the requested thread")
	}
	return identity, nil
}

func (c *Codex) Turn(session string, route config.Route, cwd, prompt string, schema schemas.Schema) (string, error) {
	if err := requireRoute(route, config.BackendCodex); err != nil {
		return "", err
	}
	answer, err := c.turn(session, route, cwd, prompt, schema)
	if err != nil {
		return "", err
	}
	return FinishTurn(answer, schema)
}

// turn starts a turn and follows it to completion; any failure after the
// turn has started interrupts it.
func (c *Codex) turn(thread string, route config.Route, cwd, prompt string, schema schemas.Schema) (string, error) {
	turn, err := c.startTurn(thread, route, cwd, prompt, schema)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(time.Duration(c.timeout) * time.Second)
	answer, err := c.awaitTurn(thread, turn, deadline)
	if err != nil {
		c.interrupt(thread, turn)
		return "", err
	}
	return answer, nil
}

// startTurn submits turn/start and returns the new turn's identity.
func (c *Codex) startTurn(thread string, route config.Route, cwd, prompt string, schema schemas.Schema) (string, error) {
	params := map[string]any{
		"threadId":       thread,
		"cwd":            cwd,
		"model":          route.Model,
		"effort":         route.Effort,
		"approvalPolicy": "never",
		"sandboxPolicy":  map[string]any{"type": "dangerFullAccess"},
		"input":          []any{map[string]any{"type": "text", "text": prompt, "text_elements": []any{}}},
	}
	if schema != nil {
		params["outputSchema"] = schema
	}
	v, err := c.rpc("turn/start", params)
	if err != nil {
		return "", err
	}
	result, _ := asObject(v)
	turnObj, _ := asObject(result["turn"])
	turn, ok := strAt(turnObj, "id")
	if !ok {
		return "", fmt.Errorf("Missing turn identity")
	}
	return turn, nil
}

// nextEvent checks cancellation and the turn deadline, then returns the
// oldest queued notification or receives the next message.
func (c *Codex) nextEvent(deadline time.Time) (map[string]any, error) {
	if c.ctx.Err() != nil {
		return nil, process.ErrSessionCancelled
	}
	if !time.Now().Before(deadline) {
		return nil, fmt.Errorf("Codex session time limit exceeded")
	}
	if event, ok := c.pending.pop(); ok {
		return event, nil
	}
	return c.receive(deadline, "Codex session time limit exceeded")
}

// awaitTurn follows the turn's events until it completes and returns the
// final agent message.
func (c *Codex) awaitTurn(thread, turn string, deadline time.Time) (string, error) {
	var answer string
	for {
		event, err := c.nextEvent(deadline)
		if err != nil {
			return "", err
		}
		params, _ := asObject(event["params"])
		if s, _ := strAt(params, "threadId"); s != thread {
			continue
		}
		if id, ok := strAt(params, "turnId"); ok && id != turn {
			continue
		}
		method, _ := strAt(event, "method")
		switch method {
		case "item/completed":
			item, _ := asObject(params["item"])
			itemType, _ := strAt(item, "type")
			phase, _ := strAt(item, "phase")
			if itemType == "agentMessage" && phase != "commentary" {
				answer, _ = strAt(item, "text")
			}
			// Only record metadata, never raw tool arguments or command
			// output from session notifications.
			itemStatus, _ := strAt(item, "status")
			if itemType == "" {
				itemType = "item"
			}
			if itemStatus == "" {
				itemStatus = "completed"
			}
			if err := c.state.Event(c.entity, "session_progress", fmt.Sprintf("%s · %s · %s", thread, itemType, itemStatus)); err != nil {
				return "", err
			}
		case "turn/completed":
			completed, _ := asObject(params["turn"])
			if s, _ := strAt(completed, "id"); s != turn {
				continue
			}
			if s, _ := strAt(completed, "status"); s != "completed" {
				encoded, _ := marshal(completed["error"])
				return "", fmt.Errorf("Codex turn did not complete successfully: %s", store.Redact(encoded))
			}
			if strings.TrimSpace(answer) == "" {
				return "", fmt.Errorf("Codex returned no final result")
			}
			return answer, nil
		}
	}
}

// interrupt sends a best-effort turn/interrupt that bypasses the owner
// context, so it still reaches the server after operator cancellation; the
// response is not awaited.
func (c *Codex) interrupt(thread, turn string) {
	c.serial++
	id := c.serial
	_, _ = process.Bounded(context.Background(), 5, "Codex interrupt timed out", func(wctx context.Context) (struct{}, error) {
		return struct{}{}, c.sendBestEffort(wctx, map[string]any{
			"id":     id,
			"method": "turn/interrupt",
			"params": map[string]any{"threadId": thread, "turnId": turn},
		})
	})
}

// Close kills the process group, closes the pipes, and joins both the line
// reader and the child wait with a bounded cleanup. Closing done makes the
// reader abandon pending sends so it always terminates. Idempotent.
func (c *Codex) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.child.Close()
		c.stdin.Close()
		c.stdout.Close()
		c.closeErr = joinOwned(c.waitCh, drained(c.lines), "Codex app-server did not exit during cleanup")
	})
	return c.closeErr
}
