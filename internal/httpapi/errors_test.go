package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func TestRequestErrorsRedactSecretsWithoutChangingTheirContract(t *testing.T) {
	app, state := testApp(t)
	cfg := config.Default()
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	revision, err := cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := state.GetRaw("settings", "config")
	if err != nil {
		t.Fatal(err)
	}
	router := Router(app, token, "", "test")
	const secret = "ghp_requestSecret0123456789"
	for _, tc := range []struct {
		name, method, path, body, prefix string
		status                           int
	}{
		{"unknown body field", "POST", "/api/baseline-checks", `{"` + secret + `":true}`, "Failed to deserialize", http.StatusUnprocessableEntity},
		{"enum value", "POST", "/api/model-catalog", `{"backend":"` + secret + `","binary":"codex"}`, "Failed to deserialize", http.StatusUnprocessableEntity},
		{"unknown patch field", "PUT", "/api/config", `{"expected_revision":"` + revision + `","config":{"` + secret + `":true}}`, "Failed to deserialize", http.StatusUnprocessableEntity},
		{"history cursor", "GET", "/api/tasks?before=" + secret, "", "Invalid query string", http.StatusBadRequest},
		{"history limit", "GET", "/api/cycles?limit=" + secret, "", "Invalid query string", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := call(t, router, tc.method, tc.path, tc.body)
			body := response.Body.String()
			if response.Code != tc.status || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.HasPrefix(body, tc.prefix) {
				t.Fatalf("response = %d %q %q", response.Code, response.Header().Get("Content-Type"), body)
			}
			if strings.Contains(body, secret) || !strings.Contains(body, "[redacted]") {
				t.Fatalf("request error was not redacted: %q", body)
			}
		})
	}
	if after, _, err := state.GetRaw("settings", "config"); err != nil || string(after) != string(before) {
		t.Fatalf("rejected requests changed saved settings: %v", err)
	}
	if latest, err := state.LatestBaseline(); err != nil || latest != nil {
		t.Fatalf("rejected requests started a baseline check: %v, %v", latest, err)
	}
}
