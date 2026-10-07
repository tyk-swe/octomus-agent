// The OpenCode HTTP/SSE adapter against the scripted OpenCode fixture: sessions, structured output and every failure mode.

package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

func TestOpenCodeSessions(t *testing.T) {
	t.Parallel()
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
	answer, err := client.Turn(session, route(), f.workspace, "Fixture prompt", nil, nil)
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
	review, err := client.Turn(fresh, route(), f.workspace, "Fixture prompt", schemas.ReviewSchema(), nil)
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
	if progress := f.progress.String(); strings.Contains(progress, "private fixture") {
		t.Fatalf("tool output leaked into progress: %s", progress)
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

func TestOpenCodeFailures(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{
		"wrong-model", "wrong-variant", "wrong-session", "wrong-message",
		"incomplete", "truncated", "failed", "missing-structured",
		"malformed-structured", "interactive", "question",
		"interactive-v2", "question-v2", "disconnect", "events-disconnect",
		"invalid-event", "invalid-json", "oversized-json", "event-404",
	} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
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
			result, err := client.Turn(session, route(), f.workspace, "Fixture prompt", schemas.ReviewSchema(), nil)
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
			case "malformed-structured":
				if !strings.HasPrefix(err.Error(), "Runner returned an invalid structured result: ") {
					t.Fatalf("%s error: %v", mode, err)
				}
			case "event-404":
				if err.Error() != `OpenCode event subscription failed with HTTP 404 Not Found: {"error": "no events"}` {
					t.Fatalf("%s error must report the HTTP status: %v", mode, err)
				}
			}
		})
	}
}
