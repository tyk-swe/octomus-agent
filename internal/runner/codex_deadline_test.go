package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestCodexTurnDeadlineIncludesStartRPC(t *testing.T) {
	const thread = "019a0000-0000-7000-8000-000000000001"
	const started = `{"id":1,"result":{"turn":{"id":"t1"}}}`
	const final = `{"method":"item/completed","params":{"threadId":"` + thread + `","turnId":"t1","item":{"type":"agentMessage","phase":"final_answer","text":"ok"}}}`
	const completed = `{"method":"turn/completed","params":{"threadId":"` + thread + `","turn":{"id":"t1","status":"completed"}}}`
	type frame struct {
		after time.Duration
		line  string
	}
	for _, tc := range []struct {
		name             string
		timeout          uint64
		limitBeforeStart bool
		frames           []frame
	}{
		{
			name:    "start and completion share one allowance",
			timeout: 3,
			frames: []frame{
				{time.Second, started},
				{2500 * time.Millisecond, final},
				{0, completed},
			},
		},
		{
			name:             "pre-response notifications cannot extend a turn",
			timeout:          1,
			limitBeforeStart: true,
			frames: []frame{
				{400 * time.Millisecond, `{"method":"thread/status/changed","params":{}}`},
				{400 * time.Millisecond, `{"method":"thread/status/changed","params":{}}`},
				{400 * time.Millisecond, `{"method":"thread/status/changed","params":{}}`},
				{400 * time.Millisecond, `{"method":"thread/status/changed","params":{}}`},
				{400 * time.Millisecond, `{"method":"thread/status/changed","params":{}}`},
				{0, started},
				{0, final},
				{0, completed},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := backlogCodex(t)
			client.timeout = tc.timeout
			lines := make(chan lineResult)
			client.lines = lines
			stop := make(chan struct{})
			done := make(chan struct{})
			startSent := make(chan struct{})
			go func() {
				defer close(done)
				for _, frame := range tc.frames {
					timer := time.NewTimer(frame.after)
					select {
					case <-timer.C:
					case <-stop:
						timer.Stop()
						return
					}
					if frame.line == started {
						close(startSent)
					}
					select {
					case lines <- lineResult{line: []byte(frame.line)}:
					case <-stop:
						return
					}
				}
			}()
			defer func() { close(stop); <-done }()
			answer, err := client.Turn(thread, codexRoute(), t.TempDir(), "prompt", nil)
			if err == nil || !strings.Contains(err.Error(), "Codex session time limit exceeded") || answer != "" {
				t.Fatalf("turn exceeded its %d-second allowance but returned %q, %v", tc.timeout, answer, err)
			}
			if tc.limitBeforeStart {
				select {
				case <-startSent:
					t.Fatal("turn kept waiting for the start response after its deadline")
				default:
				}
			} else if client.serial != 2 {
				t.Fatal("the accepted turn was not interrupted after its deadline")
			}
		})
	}
}

func TestCodexReleaseStopsUnacknowledgedTurn(t *testing.T) {
	for _, cancelTurn := range []bool{false, true} {
		name := "deadline"
		if cancelTurn {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := codexFixture(t)
			f.cfg.SessionTimeoutSeconds = 1
			if cancelTurn {
				f.cfg.SessionTimeoutSeconds = 60
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clients := New(ctx, f.cfg, DefaultConnector(f.state, "fixture", sandbox.Host{}))
			defer clients.Close()
			session, err := clients.Start(codexRoute(), f.workspace, nil)
			if err != nil {
				t.Fatal(err)
			}
			f.mode("codex", "hold-start")
			client, err := clients.Client(codexRoute().Backend, f.workspace)
			if err != nil {
				t.Fatal(err)
			}
			turn := turnIn(client, session, codexRoute(), f.workspace, "Fixture prompt", nil)
			if !testutil.WaitUntil(5*time.Second, func() bool { return f.exists("codex-entered") }) {
				t.Fatal("the peer did not receive turn/start")
			}
			if cancelTurn {
				cancel()
			}
			result, err := await(turn, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if result.err == nil || result.answer != "" {
				t.Fatalf("unacknowledged turn returned successful evidence: %+v", result)
			}
			if cancelTurn {
				if !errors.Is(result.err, process.ErrSessionCancelled) {
					t.Fatalf("lost cancellation: %v", result.err)
				}
			} else if !strings.Contains(result.err.Error(), "Codex session time limit exceeded") {
				t.Fatalf("lost turn deadline: %v", result.err)
			}
			if err := clients.Release(); err != nil {
				t.Fatalf("release: %v", err)
			}
			data, err := os.ReadFile(f.path("codex-held-pids.json"))
			if err != nil {
				t.Fatal(err)
			}
			var pids []int
			if err := json.Unmarshal(data, &pids); err != nil || len(pids) != 2 {
				t.Fatalf("invalid fixture PIDs: %s, %v", data, err)
			}
			if !testutil.WaitUntil(5*time.Second, func() bool { return !alive(pids[0]) && !alive(pids[1]) }) {
				t.Fatal("release left the unacknowledged turn or its child alive")
			}
			if f.exists("codex-interrupts.jsonl") {
				t.Fatal("a turn without an acknowledged ID must be stopped through its owner")
			}
		})
	}
}
