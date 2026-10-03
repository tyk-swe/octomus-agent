package httpapi

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestRecoveryControlConflictsThroughHTTP(t *testing.T) {
	app, state, cfg := baselineFixture(t)
	t.Cleanup(app.Shutdown)
	for _, role := range config.Roles() {
		cfg.Roles[role] = config.NewRoute("gpt-6-astra", "medium")
	}
	for _, tier := range config.Tiers() {
		cfg.Tiers[tier] = config.NewRoute("gpt-6-astra", "medium")
	}
	cfg.RepairRoute = config.NewRoute("gpt-6-astra", "medium")
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	control := model.DefaultControl()
	control.NextCycleAt = time.Now().Add(time.Hour).Unix()
	message := "earlier planning failure"
	control.Error = &message
	if err := state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	orphan := cycleRecord("orphan-audit")
	orphan.Mode, orphan.Status = model.CycleModeAudit, model.CycleRunning
	if err := state.Put("cycle", orphan.ID, orphan); err != nil {
		t.Fatal(err)
	}
	sql := func(statement string) {
		t.Helper()
		if err := state.Snapshot(func(conn *sql.Conn) error {
			_, err := conn.ExecContext(store.Background(), statement)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	sql(`CREATE TEMP TRIGGER refuse_control_recovery BEFORE UPDATE ON records
		WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')='interrupted'
		BEGIN SELECT RAISE(ABORT, 'synthetic HTTP recovery refusal bearer fixturehttpsecret123'); END`)
	if err := app.Tick(); err == nil || !strings.Contains(err.Error(), "synthetic HTTP recovery refusal") {
		t.Fatalf("expected direct recovery write refusal: %v", err)
	}
	router := Router(app, token, "", "test")
	response := call(t, router, "GET", "/api/state", "")
	body := decode(t, response)
	cause, ok := body["recovery_error"].(string)
	if response.Code != http.StatusOK || !ok || !strings.Contains(cause, "synthetic HTTP recovery refusal") || strings.Contains(cause, "fixturehttpsecret123") || body["status"] != "unhealthy" || body["cycle_active"] != true || body["active_cycle_mode"] != "audit" {
		t.Fatalf("recovery was not visible and redacted before failure reporting: %d %s", response.Code, response.Body.String())
	}
	for _, action := range []string{"audit", "cycle", "resume"} {
		response := call(t, router, "POST", "/api/control/"+action, "{}")
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "recovery") {
			t.Errorf("%s did not report a recovery conflict: %d %s", action, response.Code, response.Body.String())
		}
		if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
			t.Fatalf("%s mutated control during recovery: %+v, %v", action, saved, err)
		}
	}
	if response := call(t, router, "POST", "/api/control/pause", "{}"); response.Code != http.StatusOK {
		t.Fatalf("pause during recovery: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, router, "POST", "/api/control/bogus", "{}"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown control during recovery: %d %s", response.Code, response.Body.String())
	}
	if cycles, err := store.List[model.Cycle](state, "cycle"); err != nil || len(cycles) != 1 || !wirejson.Equal(cycles[0], orphan) {
		t.Fatalf("refused controls changed cycles: %+v, %v", cycles, err)
	}
	sql("DROP TRIGGER refuse_control_recovery")
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if response := call(t, router, "GET", "/api/state", ""); response.Code != http.StatusOK || decode(t, response)["recovery_error"] != nil {
		t.Fatalf("healthy recovery remained blocked: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, router, "POST", "/api/control/cycle", "{}"); response.Code != http.StatusOK || decode(t, response)["mode"] != "run_once" {
		t.Fatalf("healthy control after recovery: %d %s", response.Code, response.Body.String())
	}
}
