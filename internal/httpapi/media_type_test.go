package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestMutationMediaTypeIsParsedBeforeChangingState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mediaType string
		accepted  bool
	}{
		{"application/json", true},
		{"Application/JSON", true},
		{"application/JSON; charset=utf-8", true},
		// RFC 9110 permits an empty parameter after a trailing semicolon.
		{"application/json;", true},
		{"application/json; charset=utf-8;", true},
		{`application/json; charset="utf-8"; profile="fixture;v=1"`, true},
		// Match mime.ParseMediaType's tolerance for identical repeated parameters.
		{"application/json; charset=utf-8; CHARSET=utf-8", true},
		{"", false},
		{"application/jsonp", false},
		{"application/json-extra", false},
		{"application/problem+json", false},
		{"application/json; not-a-parameter", false},
		{`application/json; charset="unterminated`, false},
		{"application/json; charset=utf-8; CHARSET=latin-1", false},
		{"application/json, application/json", false},
	} {
		t.Run(tc.mediaType, func(t *testing.T) {
			t.Parallel()
			app, state := testApp(t)
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			if err := state.SaveControl(control); err != nil {
				t.Fatal(err)
			}
			before, _, err := state.GetRaw("settings", "control")
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/api/control/pause", strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", tc.mediaType)
			response := httptest.NewRecorder()
			Router(app, token, "", "test").ServeHTTP(response, req)
			if response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("response content type = %q", response.Header().Get("Content-Type"))
			}
			events, err := state.Events(nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.accepted {
				live, err := app.Control()
				if response.Code != http.StatusOK || err != nil || !live.Paused || len(events) != 1 || events[0].Kind != "operator" {
					t.Fatalf("valid media type did not pause once: response=%d control=%+v events=%+v err=%v", response.Code, live, events, err)
				}
				return
			}
			if response.Code != http.StatusUnsupportedMediaType || decode(t, response)["error"] != "Use application/json" {
				t.Errorf("invalid media type response = %d %s", response.Code, response.Body.String())
			}
			after, _, err := state.GetRaw("settings", "control")
			if err != nil || !bytes.Equal(before, after) || len(events) != 0 {
				t.Fatalf("rejected media type mutated control or events: %v, %+v", err, events)
			}
		})
	}
}

func TestMutationRejectsMultipleContentTypeFieldsBeforeChangingState(t *testing.T) {
	t.Parallel()
	for _, values := range [][]string{
		{"application/json", "text/plain"},
		{"text/plain", "application/json"},
		{"application/json", "application/json"},
		{"application/json", ""},
	} {
		t.Run(strings.Join(values, ","), func(t *testing.T) {
			t.Parallel()
			app, state := testApp(t)
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			if err := state.SaveControl(control); err != nil {
				t.Fatal(err)
			}
			before, _, err := state.GetRaw("settings", "control")
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/api/control/pause", strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+token)
			for _, value := range values {
				req.Header.Add("Content-Type", value)
			}
			response := httptest.NewRecorder()
			Router(app, token, "", "test").ServeHTTP(response, req)
			if response.Code != http.StatusUnsupportedMediaType || decode(t, response)["error"] != "Use application/json" {
				t.Errorf("multiple content types response = %d %s", response.Code, response.Body.String())
			}
			after, _, err := state.GetRaw("settings", "control")
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("multiple content types mutated control: %v", err)
			}
			if events, err := state.Events(nil); err != nil || len(events) != 0 {
				t.Fatalf("multiple content types wrote events: %+v, %v", events, err)
			}
		})
	}
}

func TestMediaTypeParsingKeepsRoutingAndAuthenticationOrder(t *testing.T) {
	t.Parallel()
	app, state := testApp(t)
	router := Router(app, token, "", "test")
	for _, tc := range []struct {
		method, path, mediaType string
		auth                    bool
		status                  int
		allow                   string
	}{
		{"POST", "/api/unknown", "application/jsonp", false, http.StatusNotFound, ""},
		{"POST", "/api/control/pause", "application/jsonp", false, http.StatusUnauthorized, ""},
		{"PUT", "/api/config", "application/jsonp", true, http.StatusUnsupportedMediaType, ""},
		{"DELETE", "/api/config", "application/jsonp", true, http.StatusUnsupportedMediaType, ""},
		{"DELETE", "/api/config", "Application/JSON", true, http.StatusMethodNotAllowed, "GET, PUT"},
		{"GET", "/api/config", "not a media type", true, http.StatusOK, ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", tc.mediaType)
		if tc.auth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != tc.status || response.Header().Get("Allow") != tc.allow {
			t.Fatalf("%s %s: status=%d Allow=%q; want %d %q", tc.method, tc.path, response.Code, response.Header().Get("Allow"), tc.status, tc.allow)
		}
	}
	if events, err := state.Events(nil); err != nil || len(events) != 0 {
		t.Fatalf("routing/authentication checks changed state: %+v, %v", events, err)
	}
}
