// The Codex app-server adapter against the scripted Codex fixture.

package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

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
	answer, err := client.Turn(session, codexRoute(), f.workspace, "Fixture prompt", nil)
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
	clients := New(context.Background(), f.cfg, DefaultConnector(f.state, "fixture", sandbox.Host{}))
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
	_, err = client.Turn(session, codexRoute(), f.workspace, "Implement this accepted task.", nil)
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
	clients := New(context.Background(), f.cfg, DefaultConnector(f.state, "fixture", sandbox.Host{}))
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
