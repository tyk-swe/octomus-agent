// Package engineapi is the small part of the Docker Engine API the sandbox broker uses, spoken over the daemon's unix
// socket without an SDK.
package engineapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIVersion is the oldest Engine API with volume subpath mounts, which sandboxes rely on.
const APIVersion = "1.45"

type Client struct {
	socket string
	http   *http.Client
}

func New(socket string) *Client {
	c := &Client{socket: socket}
	c.http = &http.Client{Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) },
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,
	}}
	return c
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", c.socket)
}

// Error is a daemon refusal with its HTTP status.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("Docker Engine: %s (HTTP %d)", e.Message, e.Status)
}

func IsNotFound(err error) bool {
	var engine *Error
	return errors.As(err, &engine) && engine.Status == http.StatusNotFound
}

// IsConflict reports a request the container's state refuses, such as killing one that is no longer running.
func IsConflict(err error) bool {
	var engine *Error
	return errors.As(err, &engine) && engine.Status == http.StatusConflict
}

func path(format string, args ...any) string {
	escaped := make([]any, len(args))
	for i, arg := range args {
		escaped[i] = url.PathEscape(fmt.Sprint(arg))
	}
	return "/v" + APIVersion + fmt.Sprintf(format, escaped...)
}

func (c *Client) do(ctx context.Context, method, target string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+target, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

func readError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var doc struct {
		Message string `json:"message"`
	}
	message := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &doc) == nil && doc.Message != "" {
		message = doc.Message
	}
	return &Error{Status: resp.StatusCode, Message: message}
}

type Version struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.do(ctx, http.MethodGet, "/version", nil, &v)
	return v, err
}

type Image struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
}

func (c *Client) ImageInspect(ctx context.Context, ref string) (Image, error) {
	var image Image
	err := c.do(ctx, http.MethodGet, path("/images/%s/json", ref), nil, &image)
	return image, err
}

type Network struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Options    map[string]string `json:"Options"`
}

func (c *Client) NetworkInspect(ctx context.Context, name string) (Network, error) {
	var network Network
	err := c.do(ctx, http.MethodGet, path("/networks/%s", name), nil, &network)
	return network, err
}

type Volume struct {
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
}

func (c *Client) VolumeInspect(ctx context.Context, name string) (Volume, error) {
	var volume Volume
	err := c.do(ctx, http.MethodGet, path("/volumes/%s", name), nil, &volume)
	return volume, err
}

// ContainerConfig is the container create body. Only fields the broker sets are modelled; everything else keeps the
// daemon default.
type ContainerConfig struct {
	Image        string            `json:"Image"`
	User         string            `json:"User"`
	Entrypoint   []string          `json:"Entrypoint"`
	Cmd          []string          `json:"Cmd"`
	WorkingDir   string            `json:"WorkingDir"`
	Env          []string          `json:"Env"`
	Labels       map[string]string `json:"Labels"`
	AttachStdin  bool              `json:"AttachStdin"`
	AttachStdout bool              `json:"AttachStdout"`
	AttachStderr bool              `json:"AttachStderr"`
	OpenStdin    bool              `json:"OpenStdin"`
	StdinOnce    bool              `json:"StdinOnce"`
	Tty          bool              `json:"Tty"`
	StopSignal   string            `json:"StopSignal"`
	HostConfig   HostConfig        `json:"HostConfig"`
}

type HostConfig struct {
	Init           bool              `json:"Init"`
	Privileged     bool              `json:"Privileged"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	CapDrop        []string          `json:"CapDrop"`
	CapAdd         []string          `json:"CapAdd"`
	SecurityOpt    []string          `json:"SecurityOpt"`
	NetworkMode    string            `json:"NetworkMode"`
	IpcMode        string            `json:"IpcMode"`
	PidMode        string            `json:"PidMode"`
	UTSMode        string            `json:"UTSMode"`
	UsernsMode     string            `json:"UsernsMode"`
	CgroupnsMode   string            `json:"CgroupnsMode"`
	PidsLimit      int64             `json:"PidsLimit"`
	Memory         int64             `json:"Memory"`
	MemorySwap     int64             `json:"MemorySwap"`
	NanoCPUs       int64             `json:"NanoCpus"`
	OomScoreAdj    int               `json:"OomScoreAdj"`
	ShmSize        int64             `json:"ShmSize"`
	Tmpfs          map[string]string `json:"Tmpfs"`
	Mounts         []Mount           `json:"Mounts"`
	Binds          []string          `json:"Binds"`
	Devices        []any             `json:"Devices"`
	Runtime        string            `json:"Runtime,omitempty"`
	AutoRemove     bool              `json:"AutoRemove"`
	LogConfig      LogConfig         `json:"LogConfig"`
	RestartPolicy  RestartPolicy     `json:"RestartPolicy"`
}

type Mount struct {
	Type          string         `json:"Type"`
	Source        string         `json:"Source"`
	Target        string         `json:"Target"`
	ReadOnly      bool           `json:"ReadOnly"`
	VolumeOptions *VolumeOptions `json:"VolumeOptions,omitempty"`
}

type VolumeOptions struct {
	NoCopy  bool   `json:"NoCopy"`
	Subpath string `json:"Subpath,omitempty"`
}

type LogConfig struct {
	Type string `json:"Type"`
}

type RestartPolicy struct {
	Name string `json:"Name"`
}

// ContainerCreate creates a container and returns its ID with the daemon's warnings. The daemon drops a setting the
// host cannot enforce, such as a swap or process limit, and says so only in a warning.
func (c *Client) ContainerCreate(ctx context.Context, name string, config ContainerConfig) (string, []string, error) {
	var created struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	target := path("/containers/create") + "?name=" + url.QueryEscape(name)
	if err := c.do(ctx, http.MethodPost, target, config, &created); err != nil {
		return "", nil, err
	}
	return created.ID, created.Warnings, nil
}

func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, path("/containers/%s/start", id), nil, nil)
}

func (c *Client) ContainerKill(ctx context.Context, id, signal string) error {
	return c.do(ctx, http.MethodPost, path("/containers/%s/kill", id)+"?signal="+url.QueryEscape(signal), nil, nil)
}

// ContainerRemove force-removes a container, by ID or name, with its anonymous volumes, and returns once it is gone.
// Named volumes are never removed. A removal already in progress elsewhere is not a failure: the daemon refuses a
// second one meanwhile, so the removal is asked for again until the container is gone, or until it fails on its own
// account. The other removal can fail and leave the container, so waiting for it alone could wait forever.
func (c *Client) ContainerRemove(ctx context.Context, id string) error {
	for delay := 50 * time.Millisecond; ; delay = min(2*delay, time.Second) {
		err := c.do(ctx, http.MethodDelete, path("/containers/%s", id)+"?force=1&v=1", nil, nil)
		if err == nil || IsNotFound(err) {
			return nil
		}
		var engine *Error
		if !errors.As(err, &engine) || engine.Status != http.StatusConflict || !strings.Contains(engine.Message, "already in progress") {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Waiting for the removal of container %s: %w", id, ctx.Err())
		case <-time.After(delay):
		}
	}
}

type ContainerState struct {
	Status    string `json:"Status"`
	Running   bool   `json:"Running"`
	OOMKilled bool   `json:"OOMKilled"`
	ExitCode  int    `json:"ExitCode"`
}

type ContainerJSON struct {
	ID     string         `json:"Id"`
	State  ContainerState `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (c *Client) ContainerInspect(ctx context.Context, id string) (ContainerJSON, error) {
	var container ContainerJSON
	err := c.do(ctx, http.MethodGet, path("/containers/%s/json", id), nil, &container)
	return container, err
}

type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

// ContainerList lists every container, running or not, that carries all the given labels.
func (c *Client) ContainerList(ctx context.Context, labels map[string]string) ([]ContainerSummary, error) {
	filter := []string{}
	for key, value := range labels {
		filter = append(filter, key+"="+value)
	}
	encoded, err := json.Marshal(map[string][]string{"label": filter})
	if err != nil {
		return nil, err
	}
	var containers []ContainerSummary
	err = c.do(ctx, http.MethodGet, path("/containers/json")+"?all=1&filters="+url.QueryEscape(string(encoded)), nil, &containers)
	return containers, err
}

type WaitResult struct {
	StatusCode int `json:"StatusCode"`
	Error      *struct {
		Message string `json:"Message"`
	} `json:"Error"`
}

// ContainerWait asynchronously reports when a started container is no longer running, including one that has
// already exited. Call it only after a successful start so the created state is not mistaken for an exit.
func (c *Client) ContainerWait(ctx context.Context, id string) (<-chan WaitResult, <-chan error) {
	results := make(chan WaitResult, 1)
	errs := make(chan error, 1)
	target := path("/containers/%s/wait", id) + "?condition=not-running"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+target, nil)
	if err != nil {
		errs <- err
		return results, errs
	}
	// A wait holds its connection open for the container's whole life, so it gets a connection of its own.
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) }}}
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			errs <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			errs <- readError(resp)
			return
		}
		var result WaitResult
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
			errs <- err
			return
		}
		results <- result
	}()
	return results, errs
}

// Attached is a hijacked attach stream: Reader yields the daemon's multiplexed stdout and stderr, Conn takes stdin.
type Attached struct {
	Conn   *net.UnixConn
	Reader *bufio.Reader
}

// CloseStdin half-closes the stream so a container created with StdinOnce sees end of input.
func (a *Attached) CloseStdin() error { return a.Conn.CloseWrite() }

func (a *Attached) Close() error { return a.Conn.Close() }

// ContainerAttach hijacks an attach stream before the container starts, so no early output is lost. ctx bounds the
// handshake; the stream itself outlives it.
func (c *Client) ContainerAttach(ctx context.Context, id string, stdin bool) (*Attached, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	unixConn := conn.(*net.UnixConn)
	query := "?stream=1&stdout=1&stderr=1"
	if stdin {
		query += "&stdin=1"
	}
	req, err := http.NewRequest(http.MethodPost, "http://docker"+path("/containers/%s/attach", id)+query, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// The handshake runs on the raw connection, so cancellation reaches it as an expired deadline.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	fail := func(err error) (*Attached, error) {
		stop()
		conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := req.Write(conn); err != nil {
		return fail(err)
	}
	reader := bufio.NewReaderSize(conn, 64<<10)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return fail(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		return fail(readError(resp))
	}
	if !stop() {
		return fail(ctx.Err())
	}
	_ = conn.SetDeadline(time.Time{})
	return &Attached{Conn: unixConn, Reader: reader}, nil
}

// Demux splits the daemon's multiplexed attach stream (for containers without a TTY) into stdout and stderr.
func Demux(r io.Reader, stdout, stderr func([]byte) error) error {
	var header [8]byte
	buf := make([]byte, 32<<10)
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		size := int(binary.BigEndian.Uint32(header[4:8]))
		sink := stdout
		switch header[0] {
		case 1:
		case 2:
			sink = stderr
		default:
			return fmt.Errorf("Unexpected attach stream %d", header[0])
		}
		for size > 0 {
			n := min(size, len(buf))
			if _, err := io.ReadFull(r, buf[:n]); err != nil {
				return err
			}
			if err := sink(buf[:n]); err != nil {
				return err
			}
			size -= n
		}
	}
}
