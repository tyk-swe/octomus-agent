package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/octomus-agent/internal/schemas"
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
