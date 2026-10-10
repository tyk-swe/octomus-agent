package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"modernc.org/sqlite"
)

func setHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("x-content-type-options", "nosniff")
	h.Set("x-frame-options", "DENY")
	h.Set("referrer-policy", "no-referrer")
	h.Set("cache-control", "no-store")
	h.Set("content-security-policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
}

func (a *api) route(method, pattern string, handle handlerFunc) {
	a.methods[pattern] = append(a.methods[pattern], method)
	a.mux.HandleFunc(method+" "+pattern, func(w http.ResponseWriter, r *http.Request) {
		status, body, err := handle(w, r)
		if err != nil {
			var be *bodyError
			if errors.As(err, &be) {
				writeBodyError(w, be)
			} else {
				writeAPIError(w, apiStatus(err), redact.Error(err))
			}
			return
		}
		writeJSON(w, status, body)
	})
}

func patternPath(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}

var probeMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

func (a *api) pathMethods(r *http.Request) (path string, allowed []string) {
	seen := map[string]struct{}{}
	for _, method := range probeMethods {
		probe := *r
		probe.Method = method
		_, pattern := a.mux.Handler(&probe)
		if pattern == "" {
			continue
		}
		candidate := patternPath(pattern)
		if path == "" {
			path = candidate
		}
		if candidate != path {
			continue
		}
		for _, registered := range a.methods[candidate] {
			if _, dup := seen[registered]; dup {
				continue
			}
			seen[registered] = struct{}{}
			allowed = append(allowed, registered)
		}
	}
	return path, allowed
}

func (a *api) serveAPI(w http.ResponseWriter, r *http.Request) {
	path, allowed := a.pathMethods(r)
	if path == "" {
		writeAPIError(w, http.StatusNotFound, "Unknown API route")
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		contentTypes := r.Header.Values("Content-Type")
		if len(contentTypes) != 1 {
			writeAPIError(w, http.StatusUnsupportedMediaType, "Use application/json")
			return
		}
		mediaType, _, err := mime.ParseMediaType(contentTypes[0])
		if err != nil || mediaType != "application/json" {
			writeAPIError(w, http.StatusUnsupportedMediaType, "Use application/json")
			return
		}
	}
	if !slices.Contains(allowed, r.Method) {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	a.mux.ServeHTTP(w, r)
}

func (a *api) authenticate(w http.ResponseWriter, r *http.Request) bool {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	digest := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(digest[:], a.tokenHash[:]) != 1 {
		time.Sleep(a.failures.delay(time.Now()))
		writeAPIError(w, http.StatusUnauthorized, "Enter the operator access token to connect.")
		return false
	}
	return true
}

func apiStatus(err error) int {
	var jc *wirejson.Error
	var sq *sqlite.Error
	switch {
	case errors.As(err, &sq) || errors.As(err, &jc):
		return http.StatusInternalServerError
	case errors.As(err, new(engine.NotFound)):
		return http.StatusNotFound
	case engine.IsActionConflict(err):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	_, settingsView := value.(*engine.SettingsView)
	generic, err := wirejson.Generic(value)
	var out []byte
	if err == nil {
		if object, ok := generic.(map[string]any); ok && settingsView {
			transforms, hasTransforms := object["transformed_fields"]
			delete(object, "transformed_fields")
			generic = redact.JSON(object)
			if redacted, ok := generic.(map[string]any); ok && hasTransforms {
				redacted["transformed_fields"] = transforms
			}
		} else {
			generic = redact.JSON(generic)
		}
		out, err = wirejson.Marshal(generic)
	}
	if err != nil {
		writeRawJSON(w, http.StatusInternalServerError, map[string]any{"error": "The response could not be encoded"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func writeRawJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, &bodyError{http.StatusRequestEntityTooLarge, "Failed to buffer the request body: length limit exceeded"}
		}
		return nil, &bodyError{http.StatusBadRequest, fmt.Sprintf("Failed to read the request body: %v", err)}
	}
	return data, nil
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	data, err := readBody(w, r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		var jc *wirejson.Error
		var ute *json.UnmarshalTypeError
		if errors.As(err, &jc) || errors.As(err, &ute) {
			return &bodyError{http.StatusUnprocessableEntity, fmt.Sprintf("Failed to deserialize the JSON body into the target type: %v", err)}
		}
		return &bodyError{http.StatusBadRequest, fmt.Sprintf("Failed to parse the request body as JSON: %v", err)}
	}
	return nil
}

type bodyError struct {
	status  int
	message string
}

func (e *bodyError) Error() string { return e.message }

func writeBodyError(w http.ResponseWriter, err *bodyError) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(err.status)
	_, _ = w.Write([]byte(redact.Text(err.message)))
}
