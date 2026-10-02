package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/notifications"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestStartupRecoveryUsesCurrentNotificationDestination(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"enable", "rotate", "same", "disable", "invalid"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, stateDBName)
			state, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if state != nil {
					_ = state.Close()
				}
			})
			received := make(chan map[string]any, 10)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				received <- payload
				w.WriteHeader(http.StatusOK)
			}))
			defer receiver.Close()
			_, destination, err := model.NotificationDestination(receiver.URL)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.GitHubRepo = "fixture/project"
			if err := state.Put("settings", "config", cfg); err != nil {
				t.Fatal(err)
			}
			putTask := func(id string, status model.Status) {
				t.Helper()
				task := model.Task{ID: id, CycleID: "cycle", Status: status,
					Config: cfg, Branch: "tyk/synthetic", CreatedAt: model.Now(), UpdatedAt: model.Now()}
				if err := state.Put("task", task.ID, task); err != nil {
					t.Fatal(err)
				}
			}
			putTask("historical-failure", model.StatusBlocked)
			if change != "enable" {
				previous := destination
				if change == "rotate" {
					previous = "previous-synthetic-destination"
				}
				if err := state.ConfigureNotifications(&previous, "enabled", nil); err != nil {
					t.Fatal(err)
				}
			}
			putTask("old-pending", model.StatusBlocked)
			putTask("interrupted-task", model.StatusExecuting)
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeRunOnce)
			control.Batch = &model.RunBatch{ID: "interrupted-run", Phase: model.BatchPhasePlanning}
			if err := state.SaveControl(control); err != nil {
				t.Fatal(err)
			}
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			state, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			currentURL := receiver.URL
			if change == "disable" {
				currentURL = ""
			} else if change == "invalid" {
				currentURL = "not-a-webhook-url"
			}
			app := engine.New(state, dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := observedServiceHTTP{Server: &http.Server{Handler: http.NotFoundHandler()}, started: make(chan net.Addr, 1)}
			var stopNotifications func()
			components := serviceComponents{
				scheduler: testServiceScheduler{recover: app.Recover, shutdown: app.Shutdown}, http: server,
				prepareWorker: func() error { return notifications.Configure(state, currentURL) },
				startWorker: func() (func(), error) {
					worker, err := notifications.Start(app.Context(), state, currentURL)
					if err != nil || worker == nil {
						return nil, err
					}
					stopNotifications = worker.Stop
					return stopNotifications, nil
				},
			}
			done := make(chan error, 1)
			go func() { done <- components.run(ctx, "127.0.0.1:0", io.Discard) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("service did not stop")
				}
			}()
			select {
			case <-server.started:
			case <-time.After(5 * time.Second):
				t.Fatal("service did not start")
			}
			recovered, err := store.Get[model.Task](state, "task", "interrupted-task")
			if err != nil || recovered == nil || recovered.Status != model.StatusBlocked {
				t.Fatalf("recovery = %+v, %v", recovered, err)
			}
			control, err = app.Control()
			if err != nil || !control.Paused || control.Error == nil {
				t.Fatalf("recovered control = %+v, %v", control, err)
			}
			want := map[string]string{"interrupted-task": "workspace_invalid", "service": "service_error_paused"}
			if change == "same" {
				want["old-pending"] = "unknown"
			} else if change == "disable" || change == "invalid" {
				clear(want)
			}
			for len(want) > 0 {
				select {
				case event := <-received:
					id, _ := event["task_id"].(string)
					if event["action"] == "inspect_service" {
						id = "service"
					}
					if category, ok := want[id]; !ok || event["category"] != category {
						t.Fatalf("unexpected or duplicate notice: %+v; remaining: %v", event, want)
					}
					delete(want, id)
				case <-time.After(5 * time.Second):
					health, err := state.NotificationHealth()
					t.Fatalf("recovery notices lost after %s: missing=%v health=%+v error=%v", change, want, health, err)
				}
			}
			if !testutil.WaitUntil(5*time.Second, func() bool {
				health, err := state.NotificationHealth()
				return err == nil && health.Pending == 0
			}) {
				t.Fatal("unexpected pending notifications after delivery")
			}
			if stopNotifications != nil {
				stopNotifications()
			}
			select {
			case event := <-received:
				t.Fatalf("unexpected notice after delivery: %+v", event)
			default:
			}
			if err := app.Recover(); err != nil {
				t.Fatal(err)
			}
			if health, err := state.NotificationHealth(); err != nil || health.Pending != 0 {
				t.Fatalf("repeat recovery re-enqueued a terminal episode: %+v, %v", health, err)
			}
		})
	}
}
