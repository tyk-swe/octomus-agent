package runner

// Owned OpenCode HTTP servers. Protocol baseline: OpenCode 1.18.30 (v2 SDK
// types).
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const OpenCodeProtocolVersion = "1.18.30"

func OpenCodeVersionWarning(version string) *string {
	if version == OpenCodeProtocolVersion {
		return nil
	}
	warning := VersionWarning(config.BackendOpencode, version,
		fmt.Sprintf("protocol baseline %s. Pin the documented CLI", OpenCodeProtocolVersion))
	return &warning
}

// OpenCode owns a `serve --hostname 127.0.0.1 --port 0` child and speaks its
// HTTP/SSE API.
type OpenCode struct {
	child     *process.GroupChild
	stdout    *os.File
	client    *http.Client
	base      string
	password  string
	agent     string
	version   string
	timeout   uint64
	ctx       context.Context
	state     *store.Store
	entity    string
	waitCh    chan error
	done      chan struct{}
	drainDone <-chan struct{}
	once      sync.Once
	closeErr  error
}

func ConnectOpenCode(ctx context.Context, cfg config.Config, cwd string, state *store.Store, entity string) (*OpenCode, error) {
	if ctx.Err() != nil {
		return nil, process.ErrSessionCancelled
	}
	passwordID, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	password := passwordID.String()
	// A unique agent cannot inherit a host agent's model, variant, or tool
	// settings.
	agentID, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	agent := "octomus-" + strings.ReplaceAll(agentID.String(), "-", "")
	policyJSON, err := marshal(workerPolicy(agent))
	if err != nil {
		return nil, err
	}
	cmd := process.Command(cfg.OpencodeBinary, cwd)
	cmd.Args = append(cmd.Args, "serve", "--hostname", "127.0.0.1", "--port", "0")
	cmd.Env = append(cmd.Env,
		"OPENCODE_SERVER_USERNAME=octomus",
		"OPENCODE_SERVER_PASSWORD="+password,
		"OPENCODE_CONFIG_CONTENT="+policyJSON,
		"OPENCODE_DISABLE_PROJECT_CONFIG=true",
		"OPENCODE_DISABLE_AUTOUPDATE=true",
		"OPENCODE_DISABLE_AUTOCOMPACT=true",
		"OPENCODE_DISABLE_TERMINAL_TITLE=true",
	)
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = stdoutW
	// Stderr is kept only as a bounded tail that explains a connect failure.
	tail := &stderrTail{}
	cmd.Stderr = tail
	cmd.WaitDelay = stderrWaitDelay
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("Could not start OpenCode; install and configure OpenCode as the service user: %w", err)
	}
	stdoutW.Close()
	child := process.NewGroupChild(cmd)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	done := make(chan struct{})
	lines := lineReader(stdoutR, 16_384, done)
	// Startup cleanup joins the line reader and the direct child under one
	// bounded wait; the startup error is returned regardless of cleanup.
	cleanup := func(err error) (*OpenCode, error) {
		close(done)
		child.Close()
		stdoutR.Close()
		_ = joinOwned(waitCh, drained(lines), "OpenCode server did not exit during cleanup")
		return nil, tail.explain(err)
	}
	base, err := process.Bounded(ctx, min(cfg.CommandTimeoutSeconds, 60), "OpenCode startup timed out", func(wctx context.Context) (string, error) {
		for range 1000 {
			select {
			case r, ok := <-lines:
				if !ok {
					return "", fmt.Errorf("OpenCode exited before server readiness")
				}
				if r.err != nil {
					return "", r.err
				}
				if endpoint, found := strings.CutPrefix(string(r.line), "opencode server listening on "); found {
					return parseReadyURL(strings.TrimSpace(endpoint))
				}
			case <-wctx.Done():
				return "", wctx.Err()
			}
		}
		return "", fmt.Errorf("OpenCode exceeded the startup output limit")
	})
	if err != nil {
		return cleanup(err)
	}
	client := newLoopbackClient()
	// Discard stdout without retaining raw logs, including after a malformed
	// stream ends the line reader.
	drainDone := discardStdout(lines, stdoutR)
	server := &OpenCode{
		child:     child,
		stdout:    stdoutR,
		client:    client,
		base:      base,
		password:  password,
		agent:     agent,
		timeout:   cfg.SessionTimeoutSeconds,
		ctx:       ctx,
		state:     state,
		entity:    entity,
		waitCh:    waitCh,
		done:      done,
		drainDone: drainDone,
	}
	// Once the server value exists, Close owns joining the transport, drain
	// goroutine, and child wait; the connect error is still the result.
	fail := func(err error) (*OpenCode, error) {
		_ = server.Close()
		return nil, tail.explain(err)
	}
	health, err := server.call("GET", "/global/health", cwd, nil, 60)
	if err != nil {
		return fail(err)
	}
	healthDoc, _ := asObject(health)
	if healthDoc["healthy"] != true {
		return fail(fmt.Errorf("OpenCode is not healthy"))
	}
	version, ok := strAt(healthDoc, "version")
	if !ok || len(version) > 100 {
		return fail(fmt.Errorf("Missing OpenCode version"))
	}
	server.version = version
	// Managed host settings can override inline config; do not run with
	// changed policy.
	effective, err := server.call("GET", "/config", cwd, nil, 60)
	if err != nil {
		return fail(err)
	}
	if !appliedPolicy(effective, agent) {
		return fail(fmt.Errorf("OpenCode did not apply Octomus unattended policy"))
	}
	return server, nil
}

// newLoopbackClient is the owned server's HTTP client: no proxy, a bounded
// dial, and redirects returned as responses instead of followed.
func newLoopbackClient() *http.Client {
	transport := &http.Transport{
		Proxy:       nil,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ProtocolSchema is the version-specific schema for contract checks, fetched
// from the owned server.
func (o *OpenCode) ProtocolSchema(cwd string) (any, error) {
	return o.call("GET", "/doc", cwd, nil, 60)
}

func (o *OpenCode) Version() string { return o.version }

// Diagnostics reports the server version against the documented protocol
// baseline.
func (o *OpenCode) Diagnostics(cwd string) (map[string]any, error) {
	return diagnosticsValue(config.BackendOpencode, o.version, OpenCodeProtocolVersion, OpenCodeVersionWarning(o.version)), nil
}

func (o *OpenCode) endpoint(path, cwd string) string {
	return o.base + path + "?directory=" + url.QueryEscape(cwd)
}

func (o *OpenCode) request(ctx context.Context, method, path, cwd string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.endpoint(path, cwd), reader)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("octomus", o.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// roundTrip sends one JSON request and reads a bounded JSON response.
func (o *OpenCode) roundTrip(ctx context.Context, method, path, cwd string, body any) (any, error) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	req, err := o.request(ctx, method, path, cwd, body)
	if err != nil {
		return nil, err
	}
	response, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenCode HTTP request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, statusError("OpenCode request failed", response, stop)
	}
	return readJSONBody(response.Body)
}

// statusSnippetLimit bounds the body bytes a failed response reports.
const statusSnippetLimit = 4096

// statusReadLimit bounds how much of a failed response's body is read. The
// read goes past the snippet so that a secret straddling the snippet's end is
// redacted whole before the snippet is cut.
const statusReadLimit = 4 * statusSnippetLimit

// statusBodyWait bounds how long a failed response's body is read, so a body
// that stalls cannot hold the caller until its own, possibly session-long,
// deadline.
const statusBodyWait = 2 * time.Second

// statusError reports a non-2xx OpenCode response with its status and a
// bounded, redacted body snippet. stop cancels the request's context; it ends
// a body read still running after statusBodyWait.
func statusError(prefix string, response *http.Response, stop context.CancelFunc) error {
	timer := time.AfterFunc(statusBodyWait, stop)
	body, err := io.ReadAll(io.LimitReader(response.Body, statusReadLimit+1))
	timer.Stop()
	text := redact.Secrets(strings.ToValidUTF8(string(body), "\uFFFD"))
	if err != nil || len(body) > statusReadLimit {
		// The read stopped inside the body, so its last word may be the start
		// of a secret that redaction cannot recognise.
		text = beforeLastWord(text)
	}
	if len(text) > statusSnippetLimit {
		// Redaction already ran, so this cut cannot expose part of a secret.
		cut := statusSnippetLimit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return fmt.Errorf("%s with HTTP %s: %s", prefix, response.Status, text)
}

// postBestEffort sends a cleanup request bounded by its own timeout,
// independent of the owner context, and ignores the outcome.
func (o *OpenCode) postBestEffort(timeout time.Duration, path, cwd string, body any) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := o.request(ctx, "POST", path, cwd, body)
	if err != nil {
		return
	}
	response, err := o.client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
}

// call is one bounded JSON round trip under the owner context.
func (o *OpenCode) call(method, path, cwd string, body any, seconds uint64) (any, error) {
	return process.Bounded(o.ctx, seconds, "OpenCode response timed out", func(wctx context.Context) (any, error) {
		return o.roundTrip(wctx, method, path, cwd, body)
	})
}

// readJSONBody reads a JSON body capped at exactly MaxMessage and rejects
// trailing data.
func readJSONBody(r io.Reader) (any, error) {
	var data []byte
	chunk := make([]byte, 32768)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			if len(data)+n > MaxMessage {
				return nil, fmt.Errorf("OpenCode message exceeds 16 MB protocol limit")
			}
			data = append(data, chunk[:n]...)
		}
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
	}
	v, err := decodeJSON(data)
	if err != nil {
		return nil, fmt.Errorf("Invalid OpenCode JSON response: %w", err)
	}
	return v, nil
}

func (o *OpenCode) Models(cwd string) ([]Model, error) {
	value, err := o.call("GET", "/provider", cwd, nil, 60)
	if err != nil {
		return nil, err
	}
	return Catalog(value)
}

func (o *OpenCode) Start(route config.Route, cwd string, resume *string) (string, error) {
	if err := requireRoute(route, config.BackendOpencode); err != nil {
		return "", err
	}
	permissions := []any{
		map[string]any{"permission": "*", "pattern": "*", "action": "allow"},
		map[string]any{"permission": "question", "pattern": "*", "action": "deny"},
		map[string]any{"permission": "task", "pattern": "*", "action": "deny"},
	}
	var session any
	if resume != nil {
		seg, err := segment(*resume)
		if err != nil {
			return "", err
		}
		session, err = o.call("GET", "/session/"+seg, cwd, nil, 60)
		if err != nil {
			return "", err
		}
	} else {
		modelID := map[string]any{"providerID": route.Provider, "id": route.Model}
		if route.Variant != nil {
			modelID["variant"] = *route.Variant
		}
		var err error
		session, err = o.call("POST", "/session", cwd, map[string]any{
			"title":      fmt.Sprintf("Octomus %s", o.entity),
			"agent":      o.agent,
			"model":      modelID,
			"permission": permissions,
		}, 60)
		if err != nil {
			return "", err
		}
	}
	doc, _ := asObject(session)
	id, ok := strAt(doc, "id")
	if !ok {
		return "", fmt.Errorf("Missing OpenCode session identity")
	}
	if _, err := segment(id); err != nil {
		return "", err
	}
	if resume != nil && id != *resume {
		return "", fmt.Errorf("OpenCode substituted the requested session")
	}
	directory, ok := strAt(doc, "directory")
	if !ok || !samePath(directory, cwd) {
		return "", fmt.Errorf("OpenCode session belongs to a different workspace")
	}
	sessionModel, _ := asObject(doc["model"])
	providerID, providerOK := strAt(sessionModel, "providerID")
	if providerOK != (route.Provider != nil) || (providerOK && providerID != *route.Provider) {
		return "", fmt.Errorf("OpenCode substituted the requested model or variant")
	}
	if s, _ := strAt(sessionModel, "id"); s != route.Model {
		return "", fmt.Errorf("OpenCode substituted the requested model or variant")
	}
	variant, variantOK := strAt(sessionModel, "variant")
	if !variantMatches(variant, variantOK, route) {
		return "", fmt.Errorf("OpenCode substituted the requested model or variant")
	}
	if !jsonEqual(doc["permission"], permissions) {
		return "", fmt.Errorf("OpenCode session has unexpected permissions")
	}
	return id, nil
}

func (o *OpenCode) Turn(session string, route config.Route, cwd, prompt string, schema schemas.Schema) (string, error) {
	if err := requireRoute(route, config.BackendOpencode); err != nil {
		return "", err
	}
	seg, err := segment(session)
	if err != nil {
		return "", err
	}
	path := "/session/" + seg
	// The structured-result check runs inside the bound so that a
	// schema-invalid result aborts the session like every other failure.
	answer, err := process.Bounded(o.ctx, o.timeout, "OpenCode session time limit exceeded", func(wctx context.Context) (string, error) {
		answer, err := o.turnInner(wctx, session, path, route, cwd, prompt, schema)
		if err != nil {
			return "", err
		}
		return FinishTurn(answer, schema)
	})
	if err != nil {
		// Independent of the cancelled owner context. Cleanup is bounded;
		// closing the adapter kills the group.
		o.postBestEffort(5*time.Second, path+"/abort", cwd, nil)
		return "", err
	}
	return answer, nil
}

// valueResult carries one decoded value or the error that ended its
// producer: the message POST's response or one SSE event.
type valueResult struct {
	value any
	err   error
}

func (o *OpenCode) turnInner(wctx context.Context, session, path string, route config.Route, cwd, prompt string, schema schemas.Schema) (string, error) {
	inner, cancel := context.WithCancel(wctx)
	defer cancel()
	// Subscribe before submitting so permission requests cannot be missed.
	req, err := o.request(inner, "GET", "/event", cwd, nil)
	if err != nil {
		return "", err
	}
	response, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("OpenCode HTTP request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", statusError("OpenCode event subscription failed", response, cancel)
	}
	// Resolve the message identity before any goroutine starts so entropy
	// failure unwinds with only the body-close and cancel defers.
	message, err := messageID()
	if err != nil {
		return "", err
	}
	events := make(chan valueResult)
	post := make(chan valueResult, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sseLoop(inner, response.Body, events)
	}()
	body := map[string]any{
		"messageID": message,
		"model":     map[string]any{"providerID": route.Provider, "modelID": route.Model},
		"agent":     o.agent,
		"system":    WorkerInstructions,
		"parts":     []any{map[string]any{"type": "text", "text": prompt}},
	}
	if route.Variant != nil {
		body["variant"] = *route.Variant
	}
	if schema != nil {
		body["format"] = map[string]any{"type": "json_schema", "schema": schema, "retryCount": 0}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		value, err := o.roundTrip(inner, "POST", path+"/message", cwd, body)
		select {
		case post <- valueResult{value, err}:
		case <-inner.Done():
		}
	}()
	defer func() {
		cancel()
		response.Body.Close()
		wg.Wait()
	}()
	var value any
	done := false
	for !done {
		select {
		case result := <-post:
			if result.err != nil {
				return "", result.err
			}
			value = result.value
			done = true
		case event, ok := <-events:
			// sseLoop never closes events; if it ever did, this keeps the
			// loop from spinning on zero values.
			if !ok {
				return "", fmt.Errorf("OpenCode event stream disconnected")
			}
			if event.err != nil {
				return "", event.err
			}
			if err := o.handleEvent(event.value, session, message, route, cwd); err != nil {
				return "", err
			}
		case <-wctx.Done():
			return "", wctx.Err()
		}
	}
	return o.validateTurn(value, session, message, route, schema)
}

// handleEvent processes one SSE event during a turn.
func (o *OpenCode) handleEvent(event any, session, message string, route config.Route, cwd string) error {
	doc, _ := asObject(event)
	props, _ := asObject(doc["properties"])
	kind, _ := strAt(doc, "type")
	eventSession, found := strAt(props, "sessionID")
	if !found {
		if info, ok := asObject(props["info"]); ok {
			eventSession, found = strAt(info, "sessionID")
		}
	}
	if !found {
		if part, ok := asObject(props["part"]); ok {
			eventSession, found = strAt(part, "sessionID")
		}
	}
	if eventSession != session {
		return nil
	}
	switch kind {
	case "permission.asked", "question.asked", "permission.v2.asked", "question.v2.asked":
		if id, ok := strAt(props, "id"); ok {
			seg, err := segment(id)
			if err != nil {
				return err
			}
			prefix := ""
			if strings.Contains(kind, ".v2.") {
				s, err := segment(session)
				if err != nil {
					return err
				}
				prefix = "/api/session/" + s
			}
			var replyPath string
			var reply any
			if strings.HasPrefix(kind, "permission.") {
				replyPath = prefix + "/permission/" + seg + "/reply"
				reply = map[string]any{"reply": "reject"}
			} else {
				replyPath = prefix + "/question/" + seg + "/reject"
				reply = map[string]any{}
			}
			o.postBestEffort(2*time.Second, replyPath, cwd, reply)
		}
		return fmt.Errorf("OpenCode requested interactive input (%s); task blocked", kind)
	case "message.updated":
		info, _ := asObject(props["info"])
		if info["role"] == "assistant" && info["parentID"] == message {
			return checkModel(info, route)
		}
	case "session.error":
		return fmt.Errorf("OpenCode session failed: %s", errorName(props["error"]))
	case "message.part.updated":
		part, _ := asObject(props["part"])
		if part["type"] == "tool" {
			state, _ := asObject(part["state"])
			status, _ := strAt(state, "status")
			if status == "completed" || status == "error" {
				return o.state.Event(o.entity, "session_progress", fmt.Sprintf("%s · tool · %s", session, status))
			}
		}
	}
	return nil
}

func (o *OpenCode) validateTurn(value any, session, message string, route config.Route, schema schemas.Schema) (string, error) {
	doc, _ := asObject(value)
	info, _ := asObject(doc["info"])
	id, ok := strAt(info, "id")
	if !ok {
		return "", fmt.Errorf("Missing OpenCode response identity")
	}
	if _, err := segment(id); err != nil {
		return "", err
	}
	if s, _ := strAt(info, "sessionID"); s != session {
		return "", fmt.Errorf("OpenCode returned an unrelated response")
	}
	if s, _ := strAt(info, "parentID"); s != message {
		return "", fmt.Errorf("OpenCode returned an unrelated response")
	}
	if s, _ := strAt(info, "role"); s != "assistant" {
		return "", fmt.Errorf("OpenCode returned an unrelated response")
	}
	if err := checkModel(info, route); err != nil {
		return "", err
	}
	if info["error"] != nil {
		return "", fmt.Errorf("OpenCode turn failed: %s", errorName(info["error"]))
	}
	timeDoc, _ := asObject(info["time"])
	if _, ok := timeDoc["completed"].(json.Number); !ok {
		return "", fmt.Errorf("OpenCode returned an incomplete result")
	}
	finish, _ := strAt(info, "finish")
	if finish != "stop" && !(schema != nil && finish == "tool-calls") {
		return "", fmt.Errorf("OpenCode turn did not complete successfully")
	}
	if schema != nil {
		output, ok := info["structured"]
		if !ok || output == nil {
			return "", fmt.Errorf("OpenCode returned no structured result")
		}
		// Turn validates the encoded result against the schema.
		return marshal(output)
	}
	parts, ok := asArray(doc["parts"])
	if !ok {
		return "", fmt.Errorf("OpenCode returned no response parts")
	}
	var answer strings.Builder
	for _, p := range parts {
		part, _ := asObject(p)
		if part["type"] != "text" || part["ignored"] == true || part["synthetic"] == true {
			continue
		}
		if s, _ := strAt(part, "sessionID"); s != session {
			return "", fmt.Errorf("OpenCode returned an unrelated response part")
		}
		if s, _ := strAt(part, "messageID"); s != id {
			return "", fmt.Errorf("OpenCode returned an unrelated response part")
		}
		text, ok := strAt(part, "text")
		if !ok {
			return "", fmt.Errorf("Invalid OpenCode text result")
		}
		if answer.Len() > 0 {
			answer.WriteByte('\n')
		}
		answer.WriteString(text)
	}
	if strings.TrimSpace(answer.String()) == "" {
		return "", fmt.Errorf("OpenCode returned no final result")
	}
	return answer.String(), nil
}

// errorName names an OpenCode error document, or "runtime error" when the
// value carries no string name.
func errorName(value any) string {
	doc, _ := asObject(value)
	if name, ok := strAt(doc, "name"); ok {
		return name
	}
	return "runtime error"
}

// Close kills the process group, closes the pipe, joins the drain and child
// wait with a bounded cleanup, and is idempotent.
func (o *OpenCode) Close() error {
	o.once.Do(func() {
		close(o.done)
		o.child.Close()
		o.stdout.Close()
		o.client.CloseIdleConnections()
		o.closeErr = joinOwned(o.waitCh, o.drainDone, "OpenCode server did not exit during cleanup")
	})
	return o.closeErr
}
