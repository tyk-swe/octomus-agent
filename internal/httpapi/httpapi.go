package httpapi

import (
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/engine"
)

const bodyLimit = 256 * 1024

type api struct {
	app       *engine.App
	tokenHash [32]byte
	failures  *authFailures
	assets    http.Handler
	mux       *http.ServeMux
	methods   map[string][]string
}

type authFailures struct {
	mu    sync.Mutex
	count uint
	last  time.Time
}

func (f *authFailures) delay(now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last.IsZero() || now.Sub(f.last) >= 60*time.Second {
		f.count = 0
	}
	shift := f.count
	if shift > 4 {
		shift = 4
	}
	delay := time.Duration(100<<shift) * time.Millisecond
	if delay > time.Second {
		delay = time.Second
	}
	f.count++
	f.last = now
	return delay
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) (int, any, error)

func Router(app *engine.App, token, assetsOverride, version string) http.Handler {
	s := &api{
		app:       app,
		tokenHash: sha256.Sum256([]byte(token)),
		failures:  &authFailures{},
		assets:    assetHandler(assetsOverride),
		mux:       http.NewServeMux(),
		methods:   map[string][]string{},
	}
	for _, route := range apiRoutes {
		s.route(route.method, route.pattern, route.handler(s))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w)
		if r.URL.Path == "/healthz" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeAPIError(w, http.StatusMethodNotAllowed, "Method not allowed")
				return
			}
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				return
			}
			writeRawJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
			return
		}
		if path, ok := strings.CutPrefix(r.URL.Path, "/api"); ok && (path == "" || strings.HasPrefix(path, "/")) {
			s.serveAPI(w, r)
			return
		}
		s.assets.ServeHTTP(w, r)
	})
}
