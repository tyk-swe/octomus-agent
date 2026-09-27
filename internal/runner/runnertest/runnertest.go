package runnertest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

type Reply struct {
	Answer string
	Effect func(cwd string) error
	Err    error
	Gate   *Gate
}

type Gate struct {
	entered     chan struct{}
	released    chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func NewGate() *Gate {
	return &Gate{entered: make(chan struct{}), released: make(chan struct{})}
}

func (g *Gate) Entered() <-chan struct{} { return g.entered }

func (g *Gate) Release() { g.releaseOnce.Do(func() { close(g.released) }) }

type CallKind string

const (
	CallConnect     CallKind = "connect"
	CallModels      CallKind = "models"
	CallDiagnostics CallKind = "diagnostics"
	CallStart       CallKind = "start"
	CallTurn        CallKind = "turn"
	CallClose       CallKind = "close"
)

type Call struct {
	Kind    CallKind
	Backend config.Backend
	Client  int
	Route   config.Route
	Cwd     string
	Resume  *string
	Session string
	Prompt  string
	Schema  schemas.Schema
}

type Script struct {
	mu          sync.Mutex
	catalog     []runner.Model
	replies     map[string][]Reply
	startErrs   map[string][]error
	connectErrs map[config.Backend][]error
	closeErrs   map[config.Backend][]error
	calls       []Call
	sessions    map[string]struct{}
	clients     int
	open        int
}

func New(catalog ...runner.Model) *Script {
	return &Script{
		catalog:     slices.Clone(catalog),
		replies:     map[string][]Reply{},
		startErrs:   map[string][]error{},
		connectErrs: map[config.Backend][]error{},
		closeErrs:   map[config.Backend][]error{},
		sessions:    map[string]struct{}{},
	}
}

func (s *Script) SetCatalog(catalog ...runner.Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catalog = slices.Clone(catalog)
}

func (s *Script) Connector() runner.Connector {
	return func(ctx context.Context, backend config.Backend, _ config.Config, cwd string) (runner.Adapter, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.clients++
		id := s.clients
		s.calls = append(s.calls, Call{Kind: CallConnect, Backend: backend, Client: id, Cwd: cwd})
		if err := pop(s.connectErrs, backend); err != nil {
			return nil, err
		}
		s.open++
		return &client{script: s, ctx: ctx, backend: backend, id: id, active: map[string]struct{}{}}, nil
	}
}

func (s *Script) Queue(route config.Route, replies ...Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := routeKey(route)
	s.replies[key] = append(s.replies[key], replies...)
}

func (s *Script) Answer(route config.Route, answers ...string) {
	for _, answer := range answers {
		s.Queue(route, Reply{Answer: answer})
	}
}

func (s *Script) FailConnect(backend config.Backend, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connectErrs[backend] = append(s.connectErrs[backend], err)
}

func (s *Script) FailStart(route config.Route, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := routeKey(route)
	s.startErrs[key] = append(s.startErrs[key], err)
}

func (s *Script) FailClose(backend config.Backend, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeErrs[backend] = append(s.closeErrs[backend], err)
}

func (s *Script) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *Script) Starts(route config.Route) []Call { return s.routed(CallStart, route) }

func (s *Script) Turns(route config.Route) []Call { return s.routed(CallTurn, route) }

func (s *Script) routed(kind CallKind, route config.Route) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := routeKey(route)
	calls := []Call{}
	for _, call := range s.calls {
		if call.Kind == kind && routeKey(call.Route) == key {
			calls = append(calls, call)
		}
	}
	return calls
}

func (s *Script) Pending(route config.Route) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.replies[routeKey(route)])
}

func (s *Script) OpenClients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

func CodexModel(name string, efforts ...string) runner.Model {
	return runner.Model{Backend: config.BackendCodex, Model: name, DisplayName: name,
		Efforts: append([]string{}, efforts...), Variants: []string{}, Available: true}
}

func OpenCodeModel(provider, name string, variants ...string) runner.Model {
	return runner.Model{Backend: config.BackendOpencode, Provider: &provider, ProviderName: &provider,
		Model: name, DisplayName: name, Efforts: []string{}, Variants: append([]string{}, variants...), Available: true}
}

func CatalogFor(routes ...config.Route) []runner.Model {
	catalog := []runner.Model{}
	for _, route := range routes {
		index := slices.IndexFunc(catalog, func(m runner.Model) bool {
			return m.Backend == route.Backend && m.Model == route.Model && deref(m.Provider) == deref(route.Provider)
		})
		if index < 0 {
			if route.Backend == config.BackendOpencode {
				m := OpenCodeModel(deref(route.Provider), route.Model)
				if route.Provider == nil {
					m.Provider, m.ProviderName = nil, nil
				}
				catalog = append(catalog, m)
			} else {
				catalog = append(catalog, CodexModel(route.Model))
			}
			index = len(catalog) - 1
		}
		m := &catalog[index]
		if route.Backend == config.BackendOpencode {
			if route.Variant != nil && !slices.Contains(m.Variants, *route.Variant) {
				m.Variants = append(m.Variants, *route.Variant)
			}
		} else if !slices.Contains(m.Efforts, route.Effort) {
			m.Efforts = append(m.Efforts, route.Effort)
		}
	}
	return catalog
}

type client struct {
	script  *Script
	ctx     context.Context
	backend config.Backend
	id      int
	active  map[string]struct{}
	closed  bool
}

func (c *client) record(call Call) {
	call.Backend = c.backend
	call.Client = c.id
	c.script.calls = append(c.script.calls, call)
}

func (c *client) usable() error {
	if c.closed {
		return errors.New("runnertest: client is closed")
	}
	return c.ctx.Err()
}

func (c *client) models() []runner.Model {
	models := []runner.Model{}
	for _, m := range c.script.catalog {
		if m.Backend == c.backend {
			models = append(models, m)
		}
	}
	return models
}

func (c *client) Models(cwd string) ([]runner.Model, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	c.record(Call{Kind: CallModels, Cwd: cwd})
	if err := c.usable(); err != nil {
		return nil, err
	}
	return c.models(), nil
}

func (c *client) Diagnose(cwd string) (runner.Diagnostics, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	c.record(Call{Kind: CallDiagnostics, Cwd: cwd})
	if err := c.usable(); err != nil {
		return runner.Diagnostics{}, err
	}
	return runner.Diagnostics{Backend: c.backend, ProtocolVersion: "scripted", Version: "scripted"}, nil
}

func (c *client) Start(route config.Route, cwd string, resume *string) (string, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	call := Call{Kind: CallStart, Route: route, Cwd: cwd}
	if resume != nil {
		value := *resume
		call.Resume = &value
	}
	session, err := func() (string, error) {
		if err := c.usable(); err != nil {
			return "", err
		}
		if err := c.checkRoute(route); err != nil {
			return "", err
		}
		if err := pop(c.script.startErrs, routeKey(route)); err != nil {
			return "", err
		}
		if resume != nil {
			if _, ok := c.script.sessions[*resume]; !ok {
				return "", fmt.Errorf("runnertest: cannot resume unknown session %s", *resume)
			}
			return *resume, nil
		}
		session := fmt.Sprintf("scripted-%s-%d", c.backend.Slug(), len(c.script.sessions)+1)
		c.script.sessions[session] = struct{}{}
		return session, nil
	}()
	call.Session = session
	c.record(call)
	if err != nil {
		return "", err
	}
	c.active[session] = struct{}{}
	return session, nil
}

func (c *client) Turn(session string, route config.Route, cwd, prompt string, schema schemas.Schema) (string, error) {
	reply, err := func() (Reply, error) {
		c.script.mu.Lock()
		defer c.script.mu.Unlock()
		c.record(Call{Kind: CallTurn, Route: route, Cwd: cwd, Session: session, Prompt: prompt, Schema: schema})
		if err := c.usable(); err != nil {
			return Reply{}, err
		}
		if err := c.checkRoute(route); err != nil {
			return Reply{}, err
		}
		if _, ok := c.active[session]; !ok {
			return Reply{}, fmt.Errorf("runnertest: session %s was not started or resumed on this client", session)
		}
		queue := c.script.replies[routeKey(route)]
		if len(queue) == 0 {
			return Reply{}, fmt.Errorf("runnertest: no scripted reply queued for route %s", route)
		}
		c.script.replies[routeKey(route)] = queue[1:]
		return queue[0], nil
	}()
	if err != nil {
		return "", err
	}
	if reply.Gate != nil {
		reply.Gate.enterOnce.Do(func() { close(reply.Gate.entered) })
		select {
		case <-reply.Gate.released:
		case <-c.ctx.Done():
			return "", fmt.Errorf("runnertest: turn cancelled: %w", c.ctx.Err())
		}
	}
	if reply.Effect != nil {
		if err := reply.Effect(cwd); err != nil {
			return "", err
		}
	}
	if reply.Err != nil {
		return "", reply.Err
	}
	return runner.FinishTurn(reply.Answer, schema)
}

func (c *client) Close() error {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	c.record(Call{Kind: CallClose})
	if c.closed {
		return nil
	}
	c.closed = true
	c.script.open--
	return pop(c.script.closeErrs, c.backend)
}

func (c *client) checkRoute(route config.Route) error {
	if route.Backend != c.backend {
		return fmt.Errorf("runnertest: %s client cannot serve route %s", c.backend.Display(), route)
	}
	return runner.ValidateRoute(route, c.models())
}

func pop[K comparable](queues map[K][]error, key K) error {
	queue := queues[key]
	if len(queue) == 0 {
		return nil
	}
	queues[key] = queue[1:]
	return queue[0]
}

func routeKey(route config.Route) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", route.Backend.Slug(), deref(route.Provider), route.Model, route.Effort, deref(route.Variant))
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
