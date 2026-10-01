package broker

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
)

// fakeEngine is a scripted Docker Engine on a unix socket: enough of the API for a broker to check its deployment
// and to create, attach to, start, wait for, kill, inspect and remove containers. A started container runs the
// test's script; hooks inject the daemon failures under test.
type fakeEngine struct {
	t        *testing.T
	socket   string
	listener net.Listener
	serving  sync.Once

	mu         sync.Mutex
	api        string
	images     map[string]string
	options    map[string]string
	containers map[string]*fakeContainer
	created    []*fakeContainer
	deletes    []string
	nextID     int
	// imageStatus, when set, is how the daemon answers every image inspection.
	imageStatus int

	// run is what a started container does. It ends the container with End or Exit unless a kill ends it first.
	run func(c *fakeContainer)
	// create may refuse or delay a create (a zero status accepts it) and adds daemon warnings.
	create func(name string, r *http.Request) (status int, warnings []string)
	// remove may refuse a removal with a status and message (a zero status removes the container).
	remove func(c *fakeContainer) (int, string)
	// kill may refuse a signal with a status and message (a zero status delivers it).
	kill func(c *fakeContainer, signal string) (int, string)
	// waitDelay holds back the reply to a wait after the container ends.
	waitDelay time.Duration
	// removeDelay is how long a removal takes; Docker refuses a second removal meanwhile.
	removeDelay time.Duration
}

type fakeContainer struct {
	e      *fakeEngine
	ID     string
	Name   string
	Spec   engineapi.ContainerConfig
	Ready  chan struct{} // closed once the container is attached and started
	exited chan struct{}

	mu       sync.Mutex
	attach   net.Conn
	attached chan struct{}
	started  bool
	running  bool
	done     bool
	removing bool
	code     int
	oom      bool
	closed   bool
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	e := &fakeEngine{t: t, api: "1.51", images: map[string]string{"octomus-sandbox:test": "sha256:first"},
		options: map[string]string{isolatedGateway: "isolated"}, containers: map[string]*fakeContainer{}}
	dir, err := os.MkdirTemp("", "octomus-engine-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e.socket = filepath.Join(dir, "docker.sock")
	if e.listener, err = net.Listen("unix", e.socket); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = e.listener.Close()
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, c := range e.created {
			c.closeOutput()
		}
	})
	return e
}

// Socket starts serving, once the test has set its hooks, and returns the engine's socket.
func (e *fakeEngine) Socket() string {
	e.serving.Do(func() {
		server := &http.Server{Handler: e.mux()}
		go func() { _ = server.Serve(e.listener) }()
		e.t.Cleanup(func() { _ = server.Close() })
	})
	return e.socket
}

// leftover adds a running container a previous broker of instance left behind.
func (e *fakeEngine) leftover(name, instance string) *fakeContainer {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	c := &fakeContainer{e: e, ID: fmt.Sprintf("c%d", e.nextID), Name: name, Ready: make(chan struct{}),
		exited: make(chan struct{}), attached: make(chan struct{}), started: true, running: true,
		Spec: engineapi.ContainerConfig{Labels: map[string]string{instanceLabel: instance}}}
	close(c.Ready)
	e.containers[c.ID] = c
	return c
}

// broker builds a broker against this engine without New's deployment checks.
func (e *fakeEngine) broker(t *testing.T, cfg Config) *Broker {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	cfg.DockerSocket = e.Socket()
	b := newBroker(cfg)
	b.info.Limits.Max = cfg.Max
	b.info.ImageID = e.images[cfg.Image]
	return b
}

func (e *fakeEngine) container(id string) *fakeContainer {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.containers[id]; ok {
		return c
	}
	for _, c := range e.containers {
		if c.Name == id {
			return c
		}
	}
	return nil
}

// Created lists every container ever created, in order.
func (e *fakeEngine) Created() []*fakeContainer {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.created)
}

// Remaining lists containers that exist now.
func (e *fakeEngine) Remaining() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := []string{}
	for _, c := range e.containers {
		names = append(names, c.Name)
	}
	return names
}

// Deletes lists the query of every removal request.
func (e *fakeEngine) Deletes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.deletes)
}

// WaitCreated waits for the nth container (from 1) to be attached and started.
func (e *fakeEngine) WaitCreated(t *testing.T, n int) *fakeContainer {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if created := e.Created(); len(created) >= n {
			select {
			case <-created[n-1].Ready:
				return created[n-1]
			case <-time.After(time.Until(deadline)):
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("container %d did not start", n)
	return nil
}

// Stdout writes one multiplexed stdout frame to the container's attach stream.
func (c *fakeContainer) Stdout(data string) { c.frame(1, data) }

// Stderr writes one multiplexed stderr frame to the container's attach stream.
func (c *fakeContainer) Stderr(data string) { c.frame(2, data) }

func (c *fakeContainer) frame(stream byte, data string) {
	<-c.attached
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	c.mu.Lock()
	conn := c.attach
	c.mu.Unlock()
	_, _ = conn.Write(append(header, data...))
}

// End closes the container's output and then ends it with code, as a process that exits does.
func (c *fakeContainer) End(code int) {
	c.closeOutput()
	c.Exit(code, false)
}

// Exit marks the container exited without closing its output.
func (c *fakeContainer) Exit(code int, oom bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return
	}
	c.running, c.done, c.code, c.oom = false, true, code, oom
	close(c.exited)
}

// Running reports whether the container was started and has not ended.
func (c *fakeContainer) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// Exited closes when the container ends.
func (c *fakeContainer) Exited() <-chan struct{} { return c.exited }

func (c *fakeContainer) closeOutput() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attach != nil && !c.closed {
		c.closed = true
		_ = c.attach.Close()
	}
}

func (e *fakeEngine) mux() http.Handler {
	prefix := "/v" + engineapi.APIVersion
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	refuse := func(w http.ResponseWriter, status int, message string) {
		reply(w, status, map[string]string{"message": message})
	}
	found := func(w http.ResponseWriter, r *http.Request) *fakeContainer {
		c := e.container(r.PathValue("id"))
		if c == nil {
			refuse(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		}
		return c
	}
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		reply(w, http.StatusOK, map[string]string{"Version": "29.0.0-fake", "ApiVersion": e.api})
	})
	mux.HandleFunc("GET "+prefix+"/images/{ref}/json", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		id, ok := e.images[r.PathValue("ref")]
		status := e.imageStatus
		e.mu.Unlock()
		if status != 0 {
			refuse(w, status, "fixture image inspection failure")
			return
		}
		if !ok {
			refuse(w, http.StatusNotFound, "No such image: "+r.PathValue("ref"))
			return
		}
		reply(w, http.StatusOK, map[string]any{"Id": id, "RepoDigests": []string{}})
	})
	mux.HandleFunc("GET "+prefix+"/networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		reply(w, http.StatusOK, map[string]any{"Name": r.PathValue("name"), "Internal": true, "Options": e.options})
	})
	mux.HandleFunc("GET "+prefix+"/volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]string{"Name": r.PathValue("name")})
	})
	mux.HandleFunc("GET "+prefix+"/containers/json", func(w http.ResponseWriter, r *http.Request) {
		var filters struct{ Label []string }
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		e.mu.Lock()
		defer e.mu.Unlock()
		list := []map[string]any{}
		for _, c := range e.containers {
			matches := true
			for _, label := range filters.Label {
				key, value, _ := strings.Cut(label, "=")
				matches = matches && c.Spec.Labels[key] == value
			}
			if matches {
				list = append(list, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "Labels": c.Spec.Labels})
			}
		}
		reply(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+prefix+"/containers/create", func(w http.ResponseWriter, r *http.Request) {
		var spec engineapi.ContainerConfig
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			refuse(w, http.StatusBadRequest, err.Error())
			return
		}
		name := r.URL.Query().Get("name")
		var warnings []string
		if e.create != nil {
			status := 0
			if status, warnings = e.create(name, r); status != 0 {
				refuse(w, status, "fixture create refused")
				return
			}
		}
		e.mu.Lock()
		e.nextID++
		c := &fakeContainer{e: e, ID: fmt.Sprintf("c%d", e.nextID), Name: name, Spec: spec, Ready: make(chan struct{}),
			exited: make(chan struct{}), attached: make(chan struct{})}
		e.containers[c.ID] = c
		e.created = append(e.created, c)
		e.mu.Unlock()
		reply(w, http.StatusCreated, map[string]any{"Id": c.ID, "Warnings": warnings})
	})
	mux.HandleFunc("POST "+prefix+"/containers/{id}/attach", func(w http.ResponseWriter, r *http.Request) {
		c := found(w, r)
		if c == nil {
			return
		}
		conn, stream, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		// The stream is the container's before the broker can see the upgrade and start it, so a container that ends
		// at once closes its output as a real one does.
		c.mu.Lock()
		c.attach = conn
		_, _ = stream.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = stream.Flush()
		c.mu.Unlock()
		close(c.attached)
		// Stdin is read and dropped, so a sandbox writing it never blocks.
		go func() { _, _ = io.Copy(io.Discard, bufio.NewReader(conn)) }()
	})
	mux.HandleFunc("POST "+prefix+"/containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		c := found(w, r)
		if c == nil {
			return
		}
		c.mu.Lock()
		c.started, c.running = true, true
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		close(c.Ready)
		run := e.run
		if run == nil {
			run = defaultRun
		}
		go run(c)
	})
	mux.HandleFunc("POST "+prefix+"/containers/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		c := found(w, r)
		if c == nil {
			return
		}
		select {
		case <-c.exited:
			time.Sleep(e.waitDelay)
			c.mu.Lock()
			code := c.code
			c.mu.Unlock()
			reply(w, http.StatusOK, map[string]int{"StatusCode": code})
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("POST "+prefix+"/containers/{id}/kill", func(w http.ResponseWriter, r *http.Request) {
		c := found(w, r)
		if c == nil {
			return
		}
		signal := r.URL.Query().Get("signal")
		if e.kill != nil {
			if status, message := e.kill(c, signal); status != 0 {
				refuse(w, status, message)
				return
			}
		}
		if !c.Running() {
			refuse(w, http.StatusConflict, "Cannot kill container: "+c.ID+": container "+c.ID+" is not running")
			return
		}
		if signal == "SIGKILL" {
			c.closeOutput()
			c.Exit(137, false)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+prefix+"/containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		c := found(w, r)
		if c == nil {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		reply(w, http.StatusOK, map[string]any{"Id": c.ID, "State": map[string]any{
			"Running": c.running, "OOMKilled": c.oom, "ExitCode": c.code}})
	})
	mux.HandleFunc("DELETE "+prefix+"/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.deletes = append(e.deletes, r.URL.RawQuery)
		e.mu.Unlock()
		c := found(w, r)
		if c == nil {
			return
		}
		if e.remove != nil {
			if status, message := e.remove(c); status != 0 {
				refuse(w, status, message)
				return
			}
		}
		c.mu.Lock()
		if c.removing {
			c.mu.Unlock()
			refuse(w, http.StatusConflict, "removal of container "+c.ID+" is already in progress")
			return
		}
		c.removing = true
		c.mu.Unlock()
		time.Sleep(e.removeDelay)
		c.closeOutput()
		c.Exit(137, false)
		e.mu.Lock()
		delete(e.containers, c.ID)
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// defaultRun answers a version probe with fixed versions and ends every other container at once with exit 0.
func defaultRun(c *fakeContainer) {
	if slices.Contains(c.Spec.Entrypoint, sandbox.ProbeVersions) {
		c.Stdout(`{"codex":"codex-fake ` + c.Spec.Image + `","opencode":"opencode-fake"}`)
	}
	c.End(0)
}

// serve runs b on a unix socket until the test ends and returns a client for it.
func serve(t *testing.T, b *Broker) *sandbox.Remote {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ob-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "sandboxd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- b.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Error("broker did not shut down")
		}
	})
	return sandbox.NewRemote(socket)
}

// syncLog is a broker log a test can read while the broker writes it.
type syncLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}
