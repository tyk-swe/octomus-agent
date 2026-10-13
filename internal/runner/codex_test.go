// The Codex app-server adapter against the scripted Codex fixture.

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

func TestCodexRejectsDuplicateMessageFields(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"id":1,"id":2,"result":{}}`,
		`{"id":1,"result":{"thread":{"id":"first","\u0069d":"second"}}}`,
	} {
		lines := make(chan lineResult, 1)
		lines <- lineResult{line: []byte(raw)}
		client := &Codex{ctx: context.Background(), lines: lines}
		value, err := client.receive(time.Now().Add(time.Second), "Response timed out")
		if err == nil || value != nil || !strings.HasPrefix(err.Error(), "Invalid app-server JSON: duplicate field ") {
			t.Errorf("receive(%s) = %v, %v; want duplicate fields refused", raw, value, err)
		}
	}
}

func TestCodexModelsAndPreResponseEvents(t *testing.T) {
	t.Parallel()
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
	answer, err := client.Turn(session, codexRoute(), f.workspace, "Fixture prompt", nil, nil)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if answer != "Fixture completed. ✓" {
		t.Fatalf("answer: %q", answer)
	}
}

func TestCodexStructuredOutput(t *testing.T) {
	t.Parallel()
	f := codexFixture(t)
	clients := New(context.Background(), f.cfg, f.connector())
	defer clients.Close()
	session, err := clients.Start(codexRoute(), f.workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	f.mode("codex", "bad-structured")
	_, err = clients.Turn(session, codexRoute(), f.workspace, "Fixture prompt", schemas.ReviewSchema(), nil)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("malformed structured output must fail: %v", err)
	}
	f.clearMode("codex")
	if err := os.WriteFile(f.workspace+"/feature.txt", []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := clients.Turn(session, codexRoute(), f.workspace, "Perform a fresh code review of the workspace.", schemas.ReviewSchema(), nil)
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

func TestCodexInteractiveRequest(t *testing.T) {
	t.Parallel()
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
	_, err = client.Turn(session, codexRoute(), f.workspace, "Implement this accepted task.", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "interactive input") {
		t.Fatalf("interactive request must block the task: %v", err)
	}
}

func TestCodexRequiresAuthentication(t *testing.T) {
	t.Parallel()
	f := codexFixture(t)
	f.mode("codex", "no-auth")
	cfg := f.cfg.Clone()
	for _, role := range config.Roles() {
		cfg.Roles[role] = codexRoute()
	}
	for _, tier := range config.Tiers() {
		cfg.Tiers[tier] = codexRoute()
	}
	cfg.RepairRoute = codexRoute()
	clients := New(context.Background(), f.cfg, f.connector())
	defer clients.Close()
	err := clients.ValidateRoutes(cfg, f.workspace, false)
	if err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("unauthenticated Codex passed route validation: %v", err)
	}
	client, err := f.connectCodex(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	if _, err := client.Diagnose(f.workspace); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("unauthenticated Codex passed diagnostics: %v", err)
	}
}

func TestCodexFirstTurnLifecycle(t *testing.T) {
	t.Parallel()
	f := codexFixture(t)
	client, err := f.connectCodex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.Start(codexRoute(), f.workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Start(codexRoute(), f.workspace, &session); !errors.Is(err, ErrSessionMissing) {
		t.Fatalf("unstarted resume was not classified as missing: %v", err)
	}
	started := 0
	checkpoint := func() error { started++; return nil }
	if _, err := client.Turn(session, codexRoute(), f.workspace, "Fixture prompt", nil, checkpoint); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("first-turn checkpoints = %d", started)
	}
	if _, err := client.Start(codexRoute(), f.workspace, &session); err != nil {
		t.Fatalf("established thread cannot resume: %v", err)
	}
	f.mode("codex", "bad-structured")
	if _, err := client.Turn(session, codexRoute(), f.workspace, "Fixture prompt", schemas.ReviewSchema(), checkpoint); err == nil {
		t.Fatal("invalid structured answer was accepted")
	}
	if started != 2 {
		t.Fatalf("failed accepted turn lost its checkpoint: %d", started)
	}
	checkpointErr := errors.New("checkpoint persistence failed")
	answer, err := client.Turn(session, codexRoute(), f.workspace, "Fixture prompt", nil, func() error { return checkpointErr })
	if !errors.Is(err, checkpointErr) || answer != "" {
		t.Fatalf("checkpoint failure returned result: %q, %v", answer, err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("checkpoint failure did not shut down cleanly: %v", err)
	}
}

func TestCodexMissingThreadClassification(t *testing.T) {
	t.Parallel()
	id := "a65c9f2e-cd5c-4d4d-a78c-56d0e43916ae"
	for _, tc := range []struct {
		name, method, code, message string
		missing                     bool
	}{
		{"matching resume", "thread/resume", "-32600", "no rollout found for thread id " + id, true},
		{"different thread", "thread/resume", "-32600", "no rollout found for thread id other", false},
		{"different request", "turn/start", "-32600", "no rollout found for thread id " + id, false},
		{"different code", "thread/resume", "-32000", "no rollout found for thread id " + id, false},
		{"wrapped text", "thread/resume", "-32600", "upstream: no rollout found for thread id " + id, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := missingCodexThread(tc.method, map[string]any{"threadId": id}, map[string]any{"code": json.Number(tc.code), "message": tc.message})
			if got != tc.missing {
				t.Fatalf("missing = %v, want %v", got, tc.missing)
			}
		})
	}
}
