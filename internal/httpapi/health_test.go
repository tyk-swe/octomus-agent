package httpapi

import (
	"net/http"
	"testing"
)

func TestHealthAllowsOnlyGetAndHead(t *testing.T) {
	app, _ := testApp(t)
	router := Router(app, token, "", "fixture-version")
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodConnect, http.MethodPost,
		http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace} {
		t.Run(method, func(t *testing.T) {
			response := request(t, router, method, "/healthz", "", false)
			if method == http.MethodGet || method == http.MethodHead {
				if response.Code != http.StatusOK {
					t.Fatalf("health status = %d", response.Code)
				}
				if method == http.MethodHead && response.Body.Len() != 0 {
					t.Fatalf("HEAD wrote a body: %q", response.Body.String())
				}
				return
			}
			if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("%s returned %d, Allow %q", method, response.Code, response.Header().Get("Allow"))
			}
		})
	}
}
