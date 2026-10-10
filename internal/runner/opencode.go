package runner

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

const OpenCodeVersion = "1.18.30"

type OpenCode struct {
	child     sandbox.Child
	stdout    io.ReadCloser
	client    *http.Client
	base      string
	password  string
	agent     string
	version   string
	timeout   uint64
	ctx       context.Context
	entity    string
	progress  Progress
	waitCh    chan error
	done      chan struct{}
	drainDone <-chan struct{}
	once      sync.Once
	closeErr  error
}

func connectOpenCode(ctx context.Context, cfg config.Config, cwd, entity string, progress Progress, box sandbox.Backend) (*OpenCode, error) {
	if ctx.Err() != nil {
		return nil, process.ErrSessionCancelled
	}
	password := uuid.NewString()
	agent := "octomus-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	policyJSON, err := marshal(workerPolicy(agent))
	if err != nil {
		return nil, err
	}
	tail := &stderrTail{}
	started, err := box.StartOpenCode(ctx, sandbox.Spec{
		Kind: wire.KindRunner, Runner: config.BackendOpencode, Binary: cfg.OpencodeBinary, Dir: cwd, Stderr: tail,
		Env: []string{
			"OPENCODE_SERVER_USERNAME=octomus",
			"OPENCODE_SERVER_PASSWORD=" + password,
			"OPENCODE_CONFIG_CONTENT=" + policyJSON,
			"OPENCODE_DISABLE_PROJECT_CONFIG=true",
			"OPENCODE_DISABLE_AUTOUPDATE=true",
			"OPENCODE_DISABLE_AUTOCOMPACT=true",
			"OPENCODE_DISABLE_TERMINAL_TITLE=true",
		},
	}, min(cfg.CommandTimeoutSeconds, 60))
	if err != nil {
		var failed *sandbox.Error
		if errors.As(err, &failed) && !failed.Started {
			return nil, fmt.Errorf("Could not start OpenCode; %s: %w", setupHint(box, "install and configure OpenCode as the service user"), failed.Err)
		}
		return nil, tail.explain(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- exitErr(started.Child.Wait()) }()
	server := &OpenCode{
		child:     started.Child,
		stdout:    started.Child.Stdout(),
		client:    newClient(started.Transport),
		base:      started.Base,
		password:  password,
		agent:     agent,
		timeout:   cfg.SessionTimeoutSeconds,
		ctx:       ctx,
		entity:    entity,
		progress:  progress,
		waitCh:    waitCh,
		done:      make(chan struct{}),
		drainDone: started.Drained,
	}
	fail := func(err error) (*OpenCode, error) {
		return nil, tail.explain(connectFailed(err, server.Close()))
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
	effective, err := server.call("GET", "/config", cwd, nil, 60)
	if err != nil {
		return fail(err)
	}
	if !appliedPolicy(effective, agent) {
		return fail(fmt.Errorf("OpenCode did not apply Octomus unattended policy"))
	}
	return server, nil
}

func newClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (o *OpenCode) protocolSchema(cwd string) (any, error) {
	return o.call("GET", "/doc", cwd, nil, 60)
}

func (o *OpenCode) Diagnose(cwd string) (Diagnostics, error) {
	return Diagnostics{
		Backend:         config.BackendOpencode,
		ProtocolVersion: OpenCodeVersion,
		Version:         o.version,
		Warning:         versionWarning(config.BackendOpencode, o.version, OpenCodeVersion),
	}, nil
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

const statusSnippetLimit = 4096

const statusReadLimit = 4 * statusSnippetLimit

const statusBodyWait = 2 * time.Second

func statusError(prefix string, response *http.Response, stop context.CancelFunc) error {
	timer := time.AfterFunc(statusBodyWait, stop)
	body, err := io.ReadAll(io.LimitReader(response.Body, statusReadLimit+1))
	timer.Stop()
	text := strings.ToValidUTF8(string(body), "\uFFFD")
	if err != nil || len(body) > statusReadLimit {
		text = redact.Fragment(text, redact.HeadWordCut)
	} else {
		text = redact.Secrets(text)
	}
	if len(text) > statusSnippetLimit {
		cut := statusSnippetLimit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return fmt.Errorf("%s with HTTP %s: %s", prefix, response.Status, text)
}

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

func (o *OpenCode) call(method, path, cwd string, body any, seconds uint64) (any, error) {
	return process.Bounded(o.ctx, time.Now().Add(time.Duration(seconds)*time.Second), "OpenCode response timed out", func(wctx context.Context) (any, error) {
		return o.roundTrip(wctx, method, path, cwd, body)
	})
}

func readJSONBody(r io.Reader) (any, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxMessage+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxMessage {
		return nil, fmt.Errorf("OpenCode message exceeds 16 MB protocol limit")
	}
	v, err := wirejson.Parse(data)
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
	if !ok || !config.SamePath(directory, cwd) {
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
	if !wirejson.Equal(doc["permission"], permissions) {
		return "", fmt.Errorf("OpenCode session has unexpected permissions")
	}
	return id, nil
}

func (o *OpenCode) Turn(session string, route config.Route, cwd, prompt string, schema schemas.Schema, started func() error) (string, error) {
	if err := requireRoute(route, config.BackendOpencode); err != nil {
		return "", err
	}
	seg, err := segment(session)
	if err != nil {
		return "", err
	}
	// OpenCode persists a resumable session at Start, before the first message.
	if started != nil {
		if err := started(); err != nil {
			return "", err
		}
	}
	path := "/session/" + seg
	answer, err := process.Bounded(o.ctx, time.Now().Add(time.Duration(o.timeout)*time.Second), "OpenCode session time limit exceeded", func(wctx context.Context) (string, error) {
		answer, err := o.turnInner(wctx, session, path, route, cwd, prompt, schema)
		if err != nil {
			return "", err
		}
		return FinishTurn(answer, schema)
	})
	if err != nil {
		o.postBestEffort(5*time.Second, path+"/abort", cwd, nil)
		return "", err
	}
	return answer, nil
}

type valueResult struct {
	value any
	err   error
}

func (o *OpenCode) turnInner(wctx context.Context, session, path string, route config.Route, cwd, prompt string, schema schemas.Schema) (string, error) {
	inner, cancel := context.WithCancel(wctx)
	defer cancel()
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
	message := messageID()
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
			eventSession, _ = strAt(part, "sessionID")
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
				return o.progress(fmt.Sprintf("%s · tool · %s", session, status))
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

func errorName(value any) string {
	doc, _ := asObject(value)
	if name, ok := strAt(doc, "name"); ok {
		return name
	}
	return "runtime error"
}

// SandboxEvidence is what the sandbox this server ran in recorded, once it is closed.
func (o *OpenCode) SandboxEvidence() *model.SandboxRecord { return sandbox.EvidenceOf(o.child) }

func (o *OpenCode) Close() error {
	o.once.Do(func() {
		close(o.done)
		o.child.Kill()
		o.stdout.Close()
		o.client.CloseIdleConnections()
		o.closeErr = joinOwned(o.waitCh, o.drainDone, "OpenCode server did not exit during cleanup")
	})
	return o.closeErr
}

var messageClock atomic.Uint64

func messageID() string {
	var random [14]byte
	_, _ = rand.Read(random[:])
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
			return fmt.Sprintf("msg_%012x%s", clock, suffix)
		}
	}
}

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
