package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// The app-server can emit notifications before a request response; the
// fixture already sends item/completed ahead of the turn/start result, so a
// completed turn exercises out-of-order pre-response events.
func TestCodexModelsAndPreResponseEvents(t *testing.T) {
	f := codexFixture(t)
	client, err := f.connectCodex(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	models, err := client.Models(f.workspace)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	names := map[string]bool{}
	for _, m := range models {
		names[m.Model] = true
	}
	if !names["gpt-6-astra"] || !names["gpt-5.6-luna"] {
		t.Fatalf("catalog missing fixture models: %v", names)
	}
	session, err := client.Start(codexRoute(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := uuid.Parse(session); err != nil {
		t.Fatalf("session identity is not a UUID: %v", err)
	}
	answer, err := client.Turn(session, codexRoute(), f.workspace, "Fixture prompt", nil)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if answer != "Fixture completed. ✓" {
		t.Fatalf("answer: %q", answer)
	}
}

// model/list pagination follows cursors across pages and accepts an empty
// final page, while a catalog whose empty pages keep a cursor fails instead of
// paging forever.
func TestCodexModelsPagination(t *testing.T) {
	f := codexFixture(t)
	client, err := f.connectCodex(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	f.mode("codex", "paged")
	models, err := client.Models(f.workspace)
	if err != nil {
		t.Fatalf("paged models: %v", err)
	}
	if len(models) != 2 || models[0].Model != "gpt-6-astra" || models[1].Model != "gpt-5.6-luna" {
		t.Fatalf("paged catalog: %+v", models)
	}
	f.mode("codex", "empty-pages")
	done := make(chan error, 1)
	go func() {
		_, err := client.Models(f.workspace)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Invalid model pagination") {
			t.Fatalf("an endless empty catalog must fail: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("model/list pagination was not bounded")
	}
}

// A Codex app-server that fails while initializing reports its redacted
// stderr with the disconnect.
func TestCodexStartupFailureReportsStderr(t *testing.T) {
	f := codexFixture(t)
	f.mode("codex", "init-failure")
	client, err := f.connectCodex(context.Background())
	if client != nil {
		client.Close()
		t.Fatal("a failed initialize must not connect")
	}
	if !errors.Is(err, errCodexDisconnected) || !strings.Contains(err.Error(), "; stderr: fixture init failure token=[redacted]") {
		t.Fatalf("connect error must explain the failure: %v", err)
	}
	if strings.Contains(err.Error(), "ghp_") {
		t.Fatalf("connect error leaked a secret: %v", err)
	}
}

// A held turn exceeds the session limit, interrupts the turn, and the
// unawaited interrupt response must not satisfy the next RPC.
func TestCodexTimeoutInterruptsTurn(t *testing.T) {
	f := codexFixture(t)
	client, err := f.connectCodex(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	session, err := client.Start(codexRoute(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	f.mode("codex", "hold")
	result, err := await(turnIn(client, session, codexRoute(), f.workspace, "Fixture prompt", nil), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "time") {
		t.Fatalf("held turn must hit the session limit: %v", result.err)
	}
	interrupt := f.codexInterrupt()
	// initialize, thread/start, and turn/start consumed request IDs 1-3.
	if interrupt["id"] != float64(4) {
		t.Fatalf("interrupt id: %v", interrupt["id"])
	}
	if interrupt["threadId"] != session {
		t.Fatalf("interrupt thread: %v", interrupt["threadId"])
	}
	if _, err := uuid.Parse(interrupt["turnId"].(string)); err != nil {
		t.Fatalf("interrupt turn: %v", interrupt["turnId"])
	}
	if models, err := client.Models(f.workspace); err != nil || len(models) == 0 {
		t.Fatalf("stale interrupt response consumed by the next RPC: %v", err)
	}
}

func TestCodexCancellationStopsTurn(t *testing.T) {
	f := codexFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	client, err := f.connectCodex(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	session, err := client.Start(codexRoute(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	f.mode("codex", "hold")
	turn := turnIn(client, session, codexRoute(), f.workspace, "Fixture prompt", nil)
	if !waitUntil(5*time.Second, func() bool { return f.exists("codex-entered") }) {
		t.Fatal("the fixture never entered the codex turn")
	}
	cancel()
	result, err := await(turn, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "cancelled") {
		t.Fatalf("cancelled turn must fail: %v", result.err)
	}
	interrupt := f.codexInterrupt()
	if interrupt["id"] != float64(4) || interrupt["threadId"] != session {
		t.Fatalf("interrupt: %v", interrupt)
	}
	if _, err := uuid.Parse(interrupt["turnId"].(string)); err != nil {
		t.Fatalf("interrupt turn: %v", interrupt["turnId"])
	}
}

// Structured answers are decoded and schema-validated through Runners.
func TestCodexStructuredOutputIsValidated(t *testing.T) {
	f := codexFixture(t)
	clients := New(context.Background(), f.cfg, DefaultConnector(f.state, "fixture"))
	defer clients.Close()
	session, err := clients.Start(codexRoute(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	f.mode("codex", "bad-structured")
	_, err = clients.Turn(session, codexRoute(), f.workspace, "Fixture prompt", schemas.ReviewSchema())
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("malformed structured output must fail: %v", err)
	}
	f.clearMode("codex")
	if err := os.WriteFile(f.workspace+"/feature.txt", []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := clients.Turn(session, codexRoute(), f.workspace, "Perform a fresh code review of the workspace.", schemas.ReviewSchema())
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(review), &value); err != nil {
		t.Fatal(err)
	}
	if value["completed"] != true || len(value["findings"].([]any)) != 0 {
		t.Fatalf("review is not clean: %s", review)
	}
}

func TestCodexResumeVerifiesThreadIdentity(t *testing.T) {
	f := codexFixture(t)
	client, err := f.connectCodex(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	session, err := client.Start(codexRoute(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := client.Turn(session, codexRoute(), f.workspace, "Implement this accepted task.", nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	resumed, err := client.Start(codexRoute(), f.workspace, &session)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed != session {
		t.Fatalf("resume changed identity: %q", resumed)
	}
	f.mode("codex", "wrong-thread")
	if _, err := client.Start(codexRoute(), f.workspace, &session); err == nil || !strings.Contains(err.Error(), "substituted") {
		t.Fatalf("a substituted thread must fail: %v", err)
	}
}

// Interactive JSON-RPC requests are answered and rejected.
func TestCodexInteractiveRequestIsRejected(t *testing.T) {
	f := codexFixture(t)
	client, err := f.connectCodex(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	session, err := client.Start(codexRoute(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := os.WriteFile(f.path("interactive"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = client.Turn(session, codexRoute(), f.workspace, "Implement this accepted task.", nil)
	if err == nil || !strings.Contains(err.Error(), "interactive input") {
		t.Fatalf("interactive request must block the task: %v", err)
	}
}

// Diagnostics report the account state and the installed version against the
// exact tested baseline.
func TestCodexDiagnosticsAndAccount(t *testing.T) {
	f := codexFixture(t)
	client, err := f.connectCodex(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	diagnostics, err := client.Diagnostics(f.workspace)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if diagnostics["backend"] != "codex" ||
		diagnostics["version"] != "codex-cli "+CodexTestedVersion ||
		diagnostics["protocol_version"] != CodexTestedVersion ||
		diagnostics["warning"] != nil {
		t.Fatalf("baseline diagnostics: %v", diagnostics)
	}
	if err := os.WriteFile(f.path("version"), []byte("0.0.0-fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diagnostics, err = client.Diagnostics(f.workspace)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if diagnostics["version"] != "codex-cli 0.0.0-fixture" || diagnostics["warning"] == nil {
		t.Fatalf("mismatched diagnostics: %v", diagnostics)
	}
	f.mode("codex", "no-auth")
	if _, err := client.Diagnostics(f.workspace); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("a missing account must fail diagnostics: %v", err)
	}
}

// pipedCodex is a Codex without a child: its protocol lines come from the
// returned channel and its requests go to a discarded pipe.
func pipedCodex(t *testing.T, ctx context.Context, timeout uint64) (*Codex, chan lineResult) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(io.Discard, r)
	}()
	t.Cleanup(func() {
		w.Close()
		<-drained
		r.Close()
	})
	lines := make(chan lineResult)
	return &Codex{stdin: w, lines: lines, timeout: timeout, ctx: ctx}, lines
}

// Each RPC message is bounded by the session timeout, not the whole call: a
// silent peer times out with the per-message wording, while notifications
// that keep arriving within the bound let a slower response still succeed.
func TestCodexRPCPerMessageBound(t *testing.T) {
	client, _ := pipedCodex(t, context.Background(), 1)
	started := time.Now()
	_, err := client.rpc("model/list", map[string]any{})
	if err == nil || err.Error() != "Codex response timed out: deadline has elapsed" {
		t.Fatalf("a silent peer must hit the per-message bound: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("per-message bound took %v", elapsed)
	}

	// Five notifications 300 ms apart outlast one session timeout in total.
	client, lines := pipedCodex(t, context.Background(), 1)
	go func() {
		for range 5 {
			time.Sleep(300 * time.Millisecond)
			lines <- lineResult{line: []byte(`{"method":"item/completed","params":{}}`)}
		}
		lines <- lineResult{line: []byte(`{"id":1,"result":{"ok":true}}`)}
	}()
	result, err := client.rpc("model/list", map[string]any{})
	if err != nil {
		t.Fatalf("notifications within the bound must keep the call alive: %v", err)
	}
	if value, _ := asObject(result); value["ok"] != true {
		t.Fatalf("result: %v", result)
	}
	if len(client.pending) != 5 {
		t.Fatalf("pre-response notifications must be queued in order: %d", len(client.pending))
	}
}

// backlogCodex is a pipedCodex whose protocol lines are all queued up front,
// so no feeder outlives an early error, with a state for progress events.
// Running out of lines reads as a disconnect.
func backlogCodex(t *testing.T, lines ...string) *Codex {
	t.Helper()
	client, _ := pipedCodex(t, context.Background(), 10)
	queued := make(chan lineResult, len(lines))
	for _, line := range lines {
		queued <- lineResult{line: []byte(line)}
	}
	close(queued)
	client.lines = queued
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	client.state = state
	client.entity = "fixture"
	return client
}

// notificationOf returns a compact notification line whose encoding is
// exactly size bytes.
func notificationOf(t *testing.T, size int) string {
	t.Helper()
	empty, err := marshal(map[string]any{"method": "n", "params": map[string]any{"p": ""}})
	if err != nil {
		t.Fatal(err)
	}
	line, err := marshal(map[string]any{"method": "n", "params": map[string]any{"p": strings.Repeat("x", size-len(empty))}})
	if err != nil || len(line) != size {
		t.Fatalf("notification size %d: %v", len(line), err)
	}
	return line
}

// Notifications that arrive before an RPC response are queued in order up to
// 10000 messages and 8 MiB of compact JSON; one message or byte more fails.
func TestCodexBacklogBounds(t *testing.T) {
	const response = `{"id":1,"result":{}}`
	small := notificationOf(t, 64)
	mebibyte := notificationOf(t, 1024*1024)
	repeat := func(line string, n int) []string {
		lines := make([]string, n)
		for i := range lines {
			lines[i] = line
		}
		return lines
	}
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
		bytes int
	}{
		{"count at the bound", repeat(small, 10_000), "", 10_000 * 64},
		{"count over the bound", repeat(small, 10_001), "Codex notification backlog exceeded", 0},
		{"bytes at the bound", repeat(mebibyte, 8), "", 8 * 1024 * 1024},
		{"bytes over the bound", append(repeat(mebibyte, 8), small), "Codex notification backlog exceeds 8 MB", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := backlogCodex(t, append(tc.lines, response)...)
			_, err := client.rpc("model/list", map[string]any{})
			if tc.want != "" {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("backlog error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a backlog at the bound must be kept: %v", err)
			}
			if len(client.pending) != len(tc.lines) || client.pendingBytes != tc.bytes {
				t.Fatalf("backlog: %d messages, %d bytes", len(client.pending), client.pendingBytes)
			}
		})
	}
}

// A stale response queued before turn/start's result is skipped by the turn,
// and the backlog drains to nothing.
func TestCodexTurnSkipsQueuedStaleResponse(t *testing.T) {
	const thread = "019a0000-0000-7000-8000-000000000001"
	client := backlogCodex(t,
		`{"id":99,"result":{}}`,
		`{"id":1,"result":{"turn":{"id":"t1"}}}`,
		`{"method":"item/completed","params":{"threadId":"`+thread+`","turnId":"t1","item":{"type":"agentMessage","phase":"final_answer","text":"ok","status":"completed"}}}`,
		`{"method":"turn/completed","params":{"threadId":"`+thread+`","turn":{"id":"t1","status":"completed"}}}`,
	)
	answer, err := client.turn(thread, codexRoute(), t.TempDir(), "prompt", nil)
	if err != nil || answer != "ok" {
		t.Fatalf("turn: %q %v", answer, err)
	}
	if len(client.pending) != 0 || client.pendingBytes != 0 {
		t.Fatalf("the backlog must drain: %d messages, %d bytes", len(client.pending), client.pendingBytes)
	}
}

// Cancelling the owner context ends a pending receive at once with the session
// cancellation error.
func TestCodexReceiveObservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client, _ := pipedCodex(t, ctx, 60)
	time.AfterFunc(100*time.Millisecond, cancel)
	started := time.Now()
	_, err := client.receive(time.Now().Add(time.Minute), "Codex session time limit exceeded")
	if !errors.Is(err, process.ErrSessionCancelled) {
		t.Fatalf("a cancelled receive must report the session cancellation: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
}

// The outbound bound applies to JSON content, not the frame terminator:
// exactly MaxMessage content bytes produce MaxMessage+1 framed bytes.
func TestOutboundFrameBound(t *testing.T) {
	pad := strings.Repeat("a", MaxMessage-8)
	payload, err := framed(map[string]any{"k": pad})
	if err != nil {
		t.Fatalf("exact bound content: %v", err)
	}
	if len(payload) != MaxMessage+1 {
		t.Fatalf("framed length: %d", len(payload))
	}
	if _, err := framed(map[string]any{"k": pad + " "}); err == nil {
		t.Fatal("content over the bound must fail")
	}
}

// A write blocked on a full pipe must unwind with the caller's context rather
// than stranding the goroutine.
func TestWriteAllHonorsCancellation(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	payload := make([]byte, 4*1024*1024)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writeAll(ctx, w, payload) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled write must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writeAll stranded on a full pipe")
	}
}

// Deterministic fixture modes cover disconnects, missing completion, and
// duplicate/stale/interleaved events.
func TestCodexEventStreams(t *testing.T) {
	for _, mode := range []string{"disconnect", "missing-completion", "duplicate", "stale", "interleaved"} {
		t.Run(mode, func(t *testing.T) {
			f := codexFixture(t)
			client, err := f.connectCodex(context.Background())
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer client.Close()
			session, err := client.Start(codexRoute(), f.workspace, nil)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			f.mode("codex", mode)
			result, err := await(turnIn(client, session, codexRoute(), f.workspace, "Fixture prompt", nil), 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "disconnect":
				if result.err == nil || !strings.Contains(result.err.Error(), "disconnect") {
					t.Fatalf("a dropped stream must fail: %v", result.err)
				}
			case "missing-completion":
				if result.err == nil || !strings.Contains(result.err.Error(), "time") {
					t.Fatalf("a missing completion must hit the session limit: %v", result.err)
				}
				f.codexInterrupt()
			default:
				if result.err != nil {
					t.Fatalf("%s events must be tolerated: %v", mode, result.err)
				}
				if result.answer != "Fixture completed. ✓" {
					t.Fatalf("%s answer: %q", mode, result.answer)
				}
			}
		})
	}
}
