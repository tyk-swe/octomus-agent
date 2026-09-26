package runner

// Owned OpenCode HTTP servers. Protocol baseline: OpenCode 1.18.30 (v2 SDK
// types).
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	whatwg "github.com/nlnwa/whatwg-url/url"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/process"
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

// workerPolicy is the unattended inline config the owned server runs under:
// sharing, updates, snapshots, LSP, formatting and compaction are off; agent
// is the default primary agent with the worker instructions and every
// permission except question and task; the helper agents are disabled.
// appliedPolicy checks the server's effective config against it.
func workerPolicy(agent string) map[string]any {
	return map[string]any{
		"share":         "disabled",
		"autoshare":     false,
		"autoupdate":    false,
		"snapshot":      false,
		"lsp":           false,
		"formatter":     false,
		"compaction":    map[string]any{"auto": false, "prune": false},
		"default_agent": agent,
		"agent": map[string]any{
			agent: map[string]any{
				"mode":       "primary",
				"prompt":     WorkerInstructions,
				"permission": map[string]any{"*": "allow", "question": "deny", "task": "deny"},
			},
			"title":      map[string]any{"disable": true},
			"summary":    map[string]any{"disable": true},
			"compaction": map[string]any{"disable": true},
		},
	}
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

// parseReadyURL accepts only a loopback root address: http scheme, literal
// 127.0.0.1, a nonzero explicit port, root path, and no user, query, or
// fragment.
func parseReadyURL(endpoint string) (string, error) {
	u, err := whatwg.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("Invalid OpenCode server address: %w", err)
	}
	port, perr := strconv.Atoi(u.Port())
	authority := endpoint
	if i := strings.Index(authority, "://"); i >= 0 {
		authority = authority[i+3:]
	}
	if i := strings.IndexByte(authority, '/'); i >= 0 {
		authority = authority[:i]
	}
	if u.Scheme() != "http" || u.Hostname() != "127.0.0.1" || perr != nil || port <= 0 ||
		u.Username() != "" || u.Password() != "" || strings.Contains(authority, "@") ||
		u.Pathname() != "/" || strings.ContainsAny(endpoint, "?#") {
		return "", fmt.Errorf("OpenCode did not bind to a local server address")
	}
	return fmt.Sprintf("http://127.0.0.1:%s", u.Port()), nil
}

// appliedPolicy validates every safety-critical field of the effective config,
// beyond the expected fields.
func appliedPolicy(effective any, agent string) bool {
	doc, ok := asObject(effective)
	if !ok {
		return false
	}
	compaction, _ := asObject(doc["compaction"])
	if doc["share"] != "disabled" || doc["autoshare"] != false || doc["autoupdate"] != false ||
		doc["snapshot"] != false || doc["lsp"] != false || doc["formatter"] != false ||
		compaction["auto"] != false || compaction["prune"] != false || doc["default_agent"] != agent {
		return false
	}
	agents, _ := asObject(doc["agent"])
	worker, _ := asObject(agents[agent])
	permission, _ := asObject(worker["permission"])
	if worker["mode"] != "primary" || worker["prompt"] != WorkerInstructions ||
		permission["*"] != "allow" || permission["question"] != "deny" || permission["task"] != "deny" {
		return false
	}
	for _, helper := range []string{"title", "summary", "compaction"} {
		entry, _ := asObject(agents[helper])
		if entry["disable"] != true {
			return false
		}
	}
	return true
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
		return nil, statusError("OpenCode request failed", response)
	}
	return readJSONBody(response.Body)
}

// statusError reports a non-2xx OpenCode response with its status and a
// bounded, redacted body snippet.
func statusError(prefix string, response *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return fmt.Errorf("%s with HTTP %s: %s", prefix, response.Status,
		store.Redact(strings.ToValidUTF8(string(snippet), "�")))
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
		return "", statusError("OpenCode event subscription failed", response)
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

// sseLoop is the owned SSE reader: arbitrary HTTP and UTF-8 fragmentation,
// CRLF, comments and other fields, multiple data lines, and each frame and
// backlog capped at exactly MaxMessage. It never closes out: it ends after
// sending exactly one error value, or earlier once ctx is done.
func sseLoop(ctx context.Context, body io.Reader, out chan<- valueResult) {
	emit := func(e valueResult) bool {
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var buffer []byte
	var data []byte
	frameBytes := 0
	chunk := make([]byte, 32768)
	for {
		for {
			end := bytes.IndexByte(buffer, '\n')
			if end < 0 {
				break
			}
			line := buffer[:end+1]
			buffer = buffer[end+1:]
			frameBytes += len(line)
			if frameBytes > MaxMessage {
				emit(valueResult{err: fmt.Errorf("OpenCode event exceeds 16 MB protocol limit")})
				return
			}
			if !utf8.Valid(line) {
				emit(valueResult{err: fmt.Errorf("Invalid OpenCode event encoding")})
				return
			}
			text := strings.TrimRight(string(line), "\r\n")
			if text == "" {
				frameBytes = 0
				if len(data) > 0 {
					value, err := decodeJSON(data)
					data = nil
					if err != nil {
						emit(valueResult{err: fmt.Errorf("Invalid OpenCode event JSON: %w", err)})
						return
					}
					if !emit(valueResult{value: value}) {
						return
					}
				}
			} else if rest, found := strings.CutPrefix(text, "data:"); found {
				rest = strings.TrimPrefix(rest, " ")
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, rest...)
			}
		}
		// Every unconsumed byte belongs to the in-progress frame; complete
		// lines are drained above before the bound applies.
		if frameBytes+len(buffer) > MaxMessage {
			emit(valueResult{err: fmt.Errorf("OpenCode event backlog exceeds 16 MB")})
			return
		}
		n, err := body.Read(chunk)
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
			continue
		}
		if err != nil {
			if err == io.EOF {
				emit(valueResult{err: fmt.Errorf("OpenCode event stream disconnected")})
			} else {
				emit(valueResult{err: err})
			}
			return
		}
	}
}

var messageClock atomic.Uint64

// messageID generates a native ordered 30-char msg_ ID: 12 lower hex chars
// from the low 48 bits of a monotonically increasing
// (milliseconds*4096+counter) clock, then 14 base62 random chars. Entropy
// failure is never silently ignored.
func messageID() (string, error) {
	var random [14]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return "", err
	}
	now := uint64(time.Now().UnixMilli()) * 4096
	for {
		previous := messageClock.Load()
		next := previous + 1
		if next < now+1 {
			next = now + 1
		}
		if messageClock.CompareAndSwap(previous, next) {
			clock := next & 0xffff_ffff_ffff
			const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
			suffix := make([]byte, 14)
			for i := range suffix {
				suffix[i] = alphabet[int(random[i])%len(alphabet)]
			}
			return fmt.Sprintf("msg_%012x%s", clock, suffix), nil
		}
	}
}

// variantMatches accepts the exact variant, or `default` when the route has
// none.
func variantMatches(reported string, ok bool, route config.Route) bool {
	if route.Variant != nil {
		return ok && reported == *route.Variant
	}
	return !ok || reported == "default"
}

func checkModel(info map[string]any, route config.Route) error {
	modelID, _ := strAt(info, "modelID")
	providerID, providerOK := strAt(info, "providerID")
	if modelID != route.Model || providerOK != (route.Provider != nil) || (providerOK && providerID != *route.Provider) {
		return fmt.Errorf("OpenCode substituted the requested model")
	}
	variant, variantOK := strAt(info, "variant")
	if !variantMatches(variant, variantOK, route) {
		return fmt.Errorf("OpenCode substituted the requested variant")
	}
	return nil
}

// segment validates an OpenCode identity and percent-encodes it like
// NON_ALPHANUMERIC.
func segment(id string) (string, error) {
	valid := id != "" && len(id) <= 256
	if valid {
		for i := 0; i < len(id); i++ {
			b := id[i]
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-') {
				valid = false
				break
			}
		}
	}
	if !valid {
		return "", fmt.Errorf("Invalid OpenCode identity")
	}
	var encoded strings.Builder
	for i := 0; i < len(id); i++ {
		b := id[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
			encoded.WriteByte(b)
		} else {
			fmt.Fprintf(&encoded, "%%%02X", b)
		}
	}
	return encoded.String(), nil
}

// jsonEqual compares two decoded JSON values by canonical compact form, like
// JSON value equality.
func jsonEqual(a, b any) bool {
	ea, err1 := marshal(a)
	eb, err2 := marshal(b)
	return err1 == nil && err2 == nil && ea == eb
}

// Catalog allowlists /provider output; the document can contain credentials
// and options that must never leave the adapter.
func Catalog(value any) ([]Model, error) {
	doc, _ := asObject(value)
	all, ok := asArray(doc["all"])
	if !ok {
		return nil, fmt.Errorf("Invalid OpenCode provider catalog")
	}
	connectedRaw, ok := asArray(doc["connected"])
	if !ok {
		return nil, fmt.Errorf("Missing OpenCode provider availability")
	}
	connected := map[string]bool{}
	for _, c := range connectedRaw {
		if s, ok := c.(string); ok {
			connected[s] = true
		}
	}
	out := []Model{}
	for _, p := range all {
		provider, _ := asObject(p)
		id, ok := strAt(provider, "id")
		if !ok {
			return nil, fmt.Errorf("Missing OpenCode provider identity")
		}
		available := connected[id]
		models, ok := asObject(provider["models"])
		if !ok {
			return nil, fmt.Errorf("Invalid OpenCode models")
		}
		modelIDs := make([]string, 0, len(models))
		for modelID := range models {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, modelID := range modelIDs {
			if len(out) >= 10000 {
				return nil, fmt.Errorf("OpenCode catalog exceeds 10000 models")
			}
			m, _ := asObject(models[modelID])
			capabilities, _ := asObject(m["capabilities"])
			toolcall := capabilities["toolcall"] == true
			input, _ := asObject(capabilities["input"])
			output, _ := asObject(capabilities["output"])
			text := input["text"] == true && output["text"] == true
			variants := []string{}
			if v, ok := asObject(m["variants"]); ok {
				for name := range v {
					variants = append(variants, name)
				}
				sort.Strings(variants)
			}
			display := modelID
			if s, ok := strAt(m, "name"); ok {
				display = s
			}
			var providerName *string
			if s, ok := strAt(provider, "name"); ok {
				providerName = stringPtr(s)
			}
			var reason *string
			switch {
			case !available:
				reason = stringPtr("Provider is not configured; configure OpenCode as the service user")
			case !toolcall || !text:
				reason = stringPtr("Model must support text and tool calling")
			}
			out = append(out, Model{
				Backend:           config.BackendOpencode,
				Provider:          stringPtr(id),
				ProviderName:      providerName,
				Model:             modelID,
				DisplayName:       display,
				Efforts:           []string{},
				Variants:          variants,
				Available:         available && toolcall && text,
				UnavailableReason: reason,
			})
		}
	}
	return out, nil
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
