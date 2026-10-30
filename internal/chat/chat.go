package chat

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Fleet is the slice of engine.Fleet the chat surface drives. Chat reaches
// state only through the engine, exactly like the HTTP API.
type Fleet interface {
	For(repo string) *engine.App
	// ForRecord is the HTTP API's owner-aware routing for ID-addressed
	// reads and actions: one repository's view never acts on another's
	// records, and a conflicting explicit selection is not-found.
	ForRecord(kind, id, selected string) (*engine.App, error)
	Apps() []*engine.App
	Entries() []store.RepositoryEntry
}

// Invocation is one operator interaction from either platform: a slash
// command's text, or a button's Action value.
type Invocation struct {
	Provider  Provider
	EventID   string // provider delivery ID, used for dedupe; may be empty in tests
	UserID    string
	ChannelID string
	ThreadID  string
	Text      string // e.g. "task abc123 --repo owner/repo"
	Action    string // button value, e.g. "confirm:<token>" or "task:retry:<id>"
}

// Button is one inline action under a response.
type Button struct {
	Label string
	Value string
}

// Response is the provider-neutral reply. Empty Text with Handled=true
// means "say nothing" (for example a duplicate delivery).
type Response struct {
	Text    string
	Buttons []Button
}

// Service routes chat invocations to engine actions.
type Service struct {
	cfg      Config
	fleet    Fleet
	store    *store.Store
	warnings io.Writer
	now      func() time.Time
	// digestPageInterval paces proactive pages to one channel. It is a
	// field so tests can use provider-scale delays without waiting a
	// real second per page.
	digestPageInterval time.Duration

	mu       sync.Mutex
	pending  map[string]*pendingAction
	warned   string
	inflight sync.WaitGroup // adapter-spawned handlers, joined at shutdown
}

// goHandle runs fn as a tracked in-flight handler. Runner.Stop joins
// these after the adapter loops end and before the process closes the
// store, so a handler can never touch chat state after shutdown.
func (s *Service) goHandle(fn func()) {
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		fn()
	}()
}

func (s *Service) waitInflight() { s.inflight.Wait() }

type pendingAction struct {
	userID  string
	expires time.Time
	run     func(context.Context) (string, error)
}

const (
	pendingTTL  = 5 * time.Minute
	maxPending  = 100
	maxTextLen  = 1900 // fits Discord's 2000-char message cap
	defaultList = 5
)

func NewService(cfg Config, fleet Fleet, st *store.Store, warnings io.Writer) *Service {
	if warnings == nil {
		warnings = io.Discard
	}
	return &Service{cfg: cfg, fleet: fleet, store: st, warnings: warnings, now: time.Now, digestPageInterval: time.Second, pending: map[string]*pendingAction{}}
}

func (s *Service) warn(err error) {
	if err == nil {
		return
	}
	message := redact.Error(err)
	s.mu.Lock()
	if message == s.warned {
		s.mu.Unlock()
		return
	}
	s.warned = message
	s.mu.Unlock()
	fmt.Fprintf(s.warnings, "WARN chat: %s\n", message)
}

// Authorized reports whether userID may drive Octomus on provider.
func (s *Service) Authorized(provider Provider, userID string) bool {
	pc := s.cfg.provider(provider)
	return pc != nil && userID != "" && pc.Operators[userID]
}

// Handle executes one invocation. handled=false means the sender is not an
// allowlisted operator and the adapter should stay silent. Duplicate
// provider deliveries return an empty response with handled=true.
func (s *Service) Handle(ctx context.Context, inv Invocation) (resp Response, handled bool) {
	if !s.Authorized(inv.Provider, inv.UserID) {
		return Response{}, false
	}
	if inv.EventID != "" && s.store != nil {
		seen, err := s.store.MarkChatEventSeen(string(inv.Provider), inv.EventID, s.now())
		if err != nil {
			// Fail closed: without a durable dedupe record a provider
			// redelivery could execute this command a second time.
			s.warn(err)
			return Response{Text: "That delivery could not be recorded safely; please try again."}, true
		}
		if seen {
			return Response{}, true
		}
	}
	var text string
	var buttons []Button
	var err error
	if inv.Action != "" {
		text, buttons, err = s.handleAction(ctx, inv)
	} else {
		text, buttons, err = s.handleCommand(ctx, inv)
	}
	if err != nil {
		text = operatorError(err)
		buttons = nil
	}
	return Response{Text: boundText(text), Buttons: buttons}, true
}

// operatorError renders an engine error the way the dashboard would: the
// engine's operator-facing messages are already capitalised fragments.
func operatorError(err error) string {
	return redact.Error(err)
}

func cleanText(text string) string {
	return strings.TrimSpace(redact.Secrets(text))
}

func boundTextLimit(text string, limit int) string {
	cleaned := cleanText(text)
	if len(cleaned) <= limit {
		return cleaned
	}
	const suffix = "\n… (truncated)"
	if limit <= len(suffix) {
		return utf8Prefix(suffix, limit)
	}
	return utf8Prefix(cleaned, limit-len(suffix)) + suffix
}

func utf8Prefix(text string, limit int) string {
	for i, r := range text {
		if i+utf8.RuneLen(r) > limit {
			return text[:i]
		}
	}
	return text
}

func boundText(text string) string {
	return boundTextLimit(text, maxTextLen)
}
