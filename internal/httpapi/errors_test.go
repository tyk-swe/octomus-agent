package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

type failedRequestBody struct{ err error }

func (b failedRequestBody) Read([]byte) (int, error) { return 0, b.err }
func (b failedRequestBody) Close() error             { return nil }

func TestBodyReadErrorsAreRedacted(t *testing.T) {
	app, _ := testApp(t)
	router := Router(app, token, "", "test")
	req := httptest.NewRequest("POST", "/api/baseline-checks", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	// TestMain installs this exact environment secret before the scrubber loads.
	req.Body = failedRequestBody{errors.New("verification_commands: Bearer fixtureReadSecret0123456789")}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || recorder.Body.String() != "Failed to read the request body: [redacted]: [redacted]" {
		t.Fatalf("read error = %d %q", recorder.Code, recorder.Body.String())
	}
}

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

func TestMalformedRequestUnicodeKeepsSafePlainTextErrors(t *testing.T) {
	app, _ := testApp(t)
	router := Router(app, token, "", "test")
	for _, body := range []string{`{"expected_revision":"\ud800"}`, "{\"expected_revision\":\"\xff\"}"} {
		response := call(t, router, "POST", "/api/baseline-checks", body)
		if response.Code != http.StatusUnprocessableEntity || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !utf8.Valid(response.Body.Bytes()) {
			t.Fatalf("malformed Unicode response = %d %q", response.Code, response.Body.String())
		}
	}
}

func TestBodyErrorsApplyTheSharedDisplayLimitAfterRedaction(t *testing.T) {
	const secret = "ghp_displayLimitSecret0123456789"
	const prefix = "Failed to read the request body: "
	recorder := httptest.NewRecorder()
	writeBodyError(recorder, &bodyError{http.StatusBadRequest, prefix + strings.Repeat("界", 16384-len(prefix)-4) + secret})
	text := recorder.Body.String()
	if utf8.RuneCountInString(text) != 16384 || !utf8.ValidString(text) {
		t.Fatalf("response has %d runes; valid UTF-8 = %t", utf8.RuneCountInString(text), utf8.ValidString(text))
	}
	if strings.Contains(text, "ghp_") || !strings.HasSuffix(text, "[red") {
		t.Fatal("display limiting must happen after secret redaction")
	}
}
