package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

// The catalog allowlists provider output and checks capabilities, provider
// availability, and variants.
func TestOpenCodeCatalogIsSafeAndChecksCapabilities(t *testing.T) {
	f := opencodeFixture(t)
	client, err := f.connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	models, err := client.Models(f.workspace)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	serialized, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret", "credential", "PRIVATE_API_KEY"} {
		if bytes.Contains(serialized, []byte(secret)) {
			t.Fatalf("catalog leaked provider material: %s", serialized)
		}
	}
	if err := ValidateRoute(route(), models); err != nil {
		t.Fatalf("route: %v", err)
	}
	selected := route()
	selected.Provider = stringPtr("alternate")
	if err := ValidateRoute(selected, models); err != nil {
		t.Fatalf("alternate provider: %v", err)
	}
	selected.Provider = stringPtr("offline")
	if err := ValidateRoute(selected, models); err == nil {
		t.Fatal("unconnected provider must fail")
	}
	selected = route()
	selected.Variant = stringPtr("invented")
	if err := ValidateRoute(selected, models); err == nil {
		t.Fatal("invented variant must fail")
	}
	selected.Variant = nil
	selected.Model = "plain-model"
	if err := ValidateRoute(selected, models); err != nil {
		t.Fatalf("variant-less model: %v", err)
	}
	selected.Model = "no-tools"
	if err := ValidateRoute(selected, models); err == nil {
		t.Fatal("a model without tool calls must be unavailable")
	}
}

// Sessions persist in the owned server across adapter restarts; structured
// review output is schema-validated.
func TestOpenCodeSessionsResumeAndStructuredOutput(t *testing.T) {
	f := opencodeFixture(t)
	client, err := f.connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	session, err := client.Start(route(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.HasPrefix(session, "ses_") {
		t.Fatalf("session identity: %q", session)
	}
	answer, err := client.Turn(session, route(), f.workspace, "Fixture prompt", nil)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if answer != "Fixture completed. ✓" {
		t.Fatalf("answer: %q", answer)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	client, err = f.connect(context.Background())
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer client.Close()
	resumed, err := client.Start(route(), f.workspace, &session)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed != session {
		t.Fatalf("resume changed identity: %q", resumed)
	}
	fresh, err := client.Start(route(), f.workspace, nil)
	if err != nil {
		t.Fatalf("fresh start: %v", err)
	}
	if fresh == session {
		t.Fatal("a new session must have a new identity")
	}
	review, err := client.Turn(fresh, route(), f.workspace, "Fixture prompt", schemas.ReviewSchema())
	if err != nil {
		t.Fatalf("structured turn: %v", err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(review), &value); err != nil {
		t.Fatal(err)
	}
	if value["completed"] != true || len(value["findings"].([]any)) != 0 {
		t.Fatalf("review is not clean: %s", review)
	}
	events, err := f.state.Events(nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(events)
	if bytes.Contains(encoded, []byte("private fixture")) {
		t.Fatalf("tool output leaked into events: %s", encoded)
	}
	f.mode("opencode", "wrong-workspace")
	if _, err := client.Start(route(), f.workspace, &session); err == nil {
		t.Fatal("a foreign workspace must fail")
	}
	f.clearMode("opencode")
	if err := os.Remove(f.path("oc-sessions") + "/" + session + ".json"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Start(route(), f.workspace, &session); err == nil {
		t.Fatal("a deleted session must fail")
	}
}

// Every failure mode fails visibly and aborts the session.
func TestOpenCodeFailuresNeverReturnSuccessfulEvidence(t *testing.T) {
	for _, mode := range []string{
		"wrong-model", "wrong-variant", "wrong-session", "wrong-message",
		"incomplete", "truncated", "failed", "missing-structured",
		"malformed-structured", "interactive", "question",
		"interactive-v2", "question-v2", "disconnect", "events-disconnect",
		"invalid-event", "invalid-json", "oversized-json",
	} {
		t.Run(mode, func(t *testing.T) {
			f := opencodeFixture(t)
			client, err := f.connect(context.Background())
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer client.Close()
			session, err := client.Start(route(), f.workspace, nil)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			f.mode("opencode", mode)
			result, err := client.Turn(session, route(), f.workspace, "Fixture prompt", schemas.ReviewSchema())
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded: %q", mode, result)
			}
			if !f.exists("opencode-aborts.jsonl") {
				t.Fatalf("%s was not aborted", mode)
			}
			switch mode {
			case "interactive", "question", "interactive-v2", "question-v2":
				if !f.exists("opencode-rejected") {
					t.Fatalf("%s request was not rejected", mode)
				}
				if !strings.Contains(err.Error(), "interactive input") {
					t.Fatalf("%s error: %v", mode, err)
				}
			case "failed":
				if err.Error() != "OpenCode turn failed: StructuredOutputError" {
					t.Fatalf("%s error must name the runner failure: %v", mode, err)
				}
			}
		})
	}
}

// A session.error event and an errored response both name the runner's error,
// falling back to "runtime error" when the error carries no string name.
func TestOpenCodeErrorsNameTheRunnerFailure(t *testing.T) {
	client := &OpenCode{}
	for _, tc := range []struct {
		name  string
		error any
		want  string
	}{
		{"named", map[string]any{"name": "ProviderAuthError", "data": map[string]any{}}, "ProviderAuthError"},
		{"unnamed", map[string]any{"data": map[string]any{}}, "runtime error"},
		{"non-string name", map[string]any{"name": json.Number("5")}, "runtime error"},
		{"non-object", "failed", "runtime error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := map[string]any{"type": "session.error", "properties": map[string]any{"sessionID": "ses_a", "error": tc.error}}
			err := client.handleEvent(event, "ses_a", "msg_parent", route(), "/workspace")
			if err == nil || err.Error() != "OpenCode session failed: "+tc.want {
				t.Fatalf("session.error: %v", err)
			}
			response := map[string]any{"info": map[string]any{
				"id": "msg_reply", "sessionID": "ses_a", "parentID": "msg_parent", "role": "assistant",
				"providerID": "fixture", "modelID": "fixture-model", "variant": "high", "error": tc.error,
			}}
			if _, err := client.validateTurn(response, "ses_a", "msg_parent", route(), nil); err == nil || err.Error() != "OpenCode turn failed: "+tc.want {
				t.Fatalf("errored response: %v", err)
			}
		})
	}
	// A session.error event without an error document still fails, and another
	// session's error is ignored.
	event := map[string]any{"type": "session.error", "properties": map[string]any{"sessionID": "ses_a"}}
	if err := client.handleEvent(event, "ses_a", "msg_parent", route(), "/workspace"); err == nil || err.Error() != "OpenCode session failed: runtime error" {
		t.Fatalf("session.error without an error document: %v", err)
	}
	if err := client.handleEvent(event, "ses_other", "msg_parent", route(), "/workspace"); err != nil {
		t.Fatalf("another session's error must be ignored: %v", err)
	}
}

// A turn without any completed text parts is not evidence.
func TestOpenCodeEmptyResultIsRejected(t *testing.T) {
	f := opencodeFixture(t)
	client, err := f.connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	session, err := client.Start(route(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	f.mode("opencode", "empty")
	if answer, err := client.Turn(session, route(), f.workspace, "Fixture prompt", nil); err == nil {
		t.Fatalf("an empty result unexpectedly succeeded: %q", answer)
	}
	if !f.exists("opencode-aborts.jsonl") {
		t.Fatal("an empty result was not aborted")
	}
}

// Cancelling a turn aborts the session; closing the adapter kills the owned
// server and its detached descendants.
func TestOpenCodeCancellationStopsOwnedServerAndDescendants(t *testing.T) {
	f := opencodeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	client, err := f.connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	session, err := client.Start(route(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	f.mode("opencode", "detached-hold")
	turn := turnIn(client, session, route(), f.workspace, "Fixture prompt", nil)
	var childPidText string
	if !waitUntil(5*time.Second, func() bool {
		var ok bool
		childPidText, ok = published(f.path("opencode-child-pid"))
		return ok
	}) {
		t.Fatal("the fixture never started its detached child")
	}
	cancel()
	result, err := await(turn, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "cancelled") {
		t.Fatalf("cancelled turn must fail: %v", result.err)
	}
	var childPid int
	if _, err := fmt.Sscanf(childPidText, "%d", &childPid); err != nil {
		t.Fatal(err)
	}
	serverData, err := os.ReadFile(f.path("opencode-pids.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(string(serverData), "\n", 2)[0]), &record); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	stopped := waitUntil(5*time.Second, func() bool { return !alive(childPid) && !alive(record.Pid) })
	if alive(childPid) {
		// Clean up the fixture's own detached group even when this regression
		// fails.
		_ = syscall.Kill(-childPid, syscall.SIGKILL)
	}
	if !stopped {
		t.Fatal("runner cleanup left a detached shell process alive")
	}
}

// Startup failures, hangs, and policy drift are bounded; a session timeout
// aborts the turn.
func TestOpenCodeStartupPolicyFailuresAndTimeoutsAreBounded(t *testing.T) {
	for _, mode := range []string{"startup-failure", "startup-hang", "wrong-policy"} {
		t.Run(mode, func(t *testing.T) {
			f := opencodeFixture(t)
			f.mode("opencode", mode)
			done := make(chan error, 1)
			go func() {
				client, err := f.connect(context.Background())
				if client != nil {
					client.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("%s must not connect", mode)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s connect was not bounded", mode)
			}
		})
	}
	f := opencodeFixture(t)
	client, err := f.connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	session, err := client.Start(route(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	f.mode("opencode", "timeout")
	result, err := await(turnIn(client, session, route(), f.workspace, "Fixture prompt", nil), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "time") {
		t.Fatalf("held turn must hit the session limit: %v", result.err)
	}
	if !f.exists("opencode-aborts.jsonl") {
		t.Fatal("a timed-out turn was not aborted")
	}
}

// The owned server's stdout stays drained for its whole life: after one
// over-long line ends the line reader, the server can keep logging.
func TestOpenCodeStdoutStaysDrainedAfterOverlongLine(t *testing.T) {
	f := opencodeFixture(t)
	f.mode("opencode", "overlong-stdout")
	client, err := f.connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	finished := make(chan error, 1)
	go func() {
		// Each request logs 8 KiB, so twenty outgrow a 64 KiB pipe.
		for range 20 {
			if _, err := client.Models(f.workspace); err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("models: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the server blocked on undrained stdout")
	}
}

// Connect failures before and after readiness report the server's redacted
// stderr; a silent failure keeps its plain message.
func TestOpenCodeStartupFailureReportsStderr(t *testing.T) {
	for mode, want := range map[string]string{
		"startup-failure":  "OpenCode exited before server readiness",
		"startup-stderr":   "OpenCode exited before server readiness; stderr: fixture startup failure token=[redacted]",
		"unhealthy-stderr": "OpenCode is not healthy; stderr: fixture health failure token=[redacted]",
	} {
		t.Run(mode, func(t *testing.T) {
			f := opencodeFixture(t)
			f.mode("opencode", mode)
			client, err := f.connect(context.Background())
			if client != nil {
				client.Close()
				t.Fatalf("%s must not connect", mode)
			}
			if err == nil || err.Error() != want {
				t.Fatalf("connect error: %v", err)
			}
		})
	}
}

// A redirect is never followed; the 3xx response fails closed.
func TestOpenCodeRedirectRefusal(t *testing.T) {
	f := opencodeFixture(t)
	client, err := f.connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	f.mode("opencode", "redirect")
	if _, err := client.Models(f.workspace); err == nil {
		t.Fatal("a redirect must fail closed")
	} else if !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect error: %v", err)
	}
}

// Diagnostics report the server version against the protocol baseline.
func TestOpenCodeDiagnostics(t *testing.T) {
	f := opencodeFixture(t)
	client, err := f.connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	diagnostics, err := client.Diagnostics(f.workspace)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if diagnostics["protocol_version"] != OpenCodeProtocolVersion || diagnostics["warning"] != nil {
		t.Fatalf("baseline diagnostics: %v", diagnostics)
	}
	client.Close()
	f.mode("opencode", "version-mismatch")
	client, err = f.connect(context.Background())
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer client.Close()
	diagnostics, err = client.Diagnostics(f.workspace)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if diagnostics["version"] != "0.0.0-fixture" || diagnostics["warning"] == nil {
		t.Fatalf("mismatched diagnostics: %v", diagnostics)
	}
}

// The exact protocol bound: 16,000,000 bytes pass, 16,000,001 fail, for line
// framing, SSE frames and backlogs, and JSON bodies.
func TestProtocolMessageBound(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	// Line framing: exactly MaxMessage bytes before the newline is accepted.
	line := make([]byte, MaxMessage)
	for i := range line {
		line[i] = 'x'
	}
	lines := lineReader(io.MultiReader(bytes.NewReader(line), bytes.NewReader([]byte("\n"))), MaxMessage, done)
	result := <-lines
	if result.err != nil || len(result.line) != MaxMessage {
		t.Fatalf("exact bound line: %v %d", result.err, len(result.line))
	}
	over := make([]byte, MaxMessage+1)
	for i := range over {
		over[i] = 'x'
	}
	lines = lineReader(bytes.NewReader(over), MaxMessage, done)
	if result := <-lines; result.err == nil {
		t.Fatal("a line over the bound must fail")
	}
	// SSE frames count every line byte including the blank terminator;
	// exactly MaxMessage is accepted.
	payload := "data: " + `{"k":"` + strings.Repeat("a", MaxMessage-2-14) + `"}`
	if len(payload)+2 != MaxMessage {
		t.Fatalf("fixture math: %d", len(payload))
	}
	sse := make(chan sseEvent, 4)
	go sseLoop(context.Background(), io.MultiReader(
		bytes.NewReader([]byte(payload)), bytes.NewReader([]byte("\n\n"))), sse)
	event := <-sse
	if event.err != nil {
		t.Fatalf("exact bound frame: %v", event.err)
	}
	payloadOver := payload + " "
	sse = make(chan sseEvent, 4)
	go sseLoop(context.Background(), bytes.NewReader([]byte(payloadOver+"\n")), sse)
	if event := <-sse; event.err == nil {
		t.Fatal("a frame over the bound must fail")
	}
	// A backlog with no newline over the bound fails too.
	sse = make(chan sseEvent, 4)
	go sseLoop(context.Background(), bytes.NewReader(over), sse)
	if event := <-sse; event.err == nil {
		t.Fatal("a backlog over the bound must fail")
	}
	// JSON bodies: exactly MaxMessage bytes of valid JSON parse.
	body := `{"k":"` + strings.Repeat("a", MaxMessage-8) + `"}`
	if len(body) != MaxMessage {
		t.Fatalf("fixture math: %d", len(body))
	}
	if _, err := readJSONBody(bytes.NewReader([]byte(body))); err != nil {
		t.Fatalf("exact bound body: %v", err)
	}
	bodyOver := `{"k":"` + strings.Repeat("a", MaxMessage-7) + `"}`
	if _, err := readJSONBody(bytes.NewReader([]byte(bodyOver))); err == nil {
		t.Fatal("a body over the bound must fail")
	}
	// Trailing data after a valid value is rejected.
	if _, err := readJSONBody(bytes.NewReader([]byte(`{} trailing`))); err == nil {
		t.Fatal("trailing JSON data must fail")
	}
}

// The same bound applies through the owned HTTP path: a response of exactly
// MaxMessage parses and one byte more fails.
func TestProtocolMessageBoundOverHTTP(t *testing.T) {
	body := `{"k":"` + strings.Repeat("a", MaxMessage-8) + `"}`
	over := `{"k":"` + strings.Repeat("a", MaxMessage-7) + `"}`
	var serve string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/over" {
			_, _ = w.Write([]byte(over))
			return
		}
		_, _ = w.Write([]byte(serve))
	}))
	defer server.Close()
	serve = body
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readJSONBody(response.Body); err != nil {
		t.Fatalf("exact bound HTTP body: %v", err)
	}
	response.Body.Close()
	response, err = http.Get(server.URL + "/over")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := readJSONBody(response.Body); err == nil {
		t.Fatal("an oversized HTTP body must fail")
	}
}
