package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	octomus "github.com/tyk-swe/octomus-agent"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/evidence"
	"github.com/tyk-swe/octomus-agent/internal/httpapi"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/notifications"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/report"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

const stateDBName = "state.db"

func printJSON(stdout io.Writer, value any) error {
	data, err := wirejson.Marshal(value)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, pretty.String())
	return err
}

type arguments struct {
	dataDir, listen                                   string
	listenAddr                                        netip.AddrPort
	assets, exportRun                                 *string
	printConfig, doctor, audit, usageReport, sandboxd bool
	egress, healthcheck                               bool
	sandbox                                           sandbox.Mode
}

func main() { os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr)) }
func run(args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	// The in-sandbox helper runs inside containers and takes its own arguments.
	if len(args) > 0 && args[0] == "--sandbox-init" {
		return sandbox.RunInit(args[1:], os.Stdin, stdout, stderr)
	}
	// Git calls its credential helper with the operation appended.
	if len(args) > 0 && args[0] == "--git-credential" {
		return gitCredential(args[1:], os.Stdin, stdout)
	}
	parsed, display, err := parse(args, env)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n\nFor more information, try '--help'.\n", err)
		return 2
	}
	if display == "help" {
		fmt.Fprint(stdout, help)
		return 0
	}
	if display == "version" {
		fmt.Fprintln(stdout, "octomus-agent "+octomus.Version)
		return 0
	}
	if parsed.printConfig {
		if err := printJSON(stdout, config.Default()); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		return 0
	}
	if parsed.healthcheck {
		return healthcheck(parsed.listen, stderr)
	}
	if parsed.egress {
		if err := runEgress(env, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		return 0
	}
	if parsed.sandboxd {
		if err := runBroker(env, stderr); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		return 0
	}
	if parsed.usageReport || parsed.exportRun != nil {
		stateDB := filepath.Join(parsed.dataDir, stateDBName)
		var value map[string]any
		var err error
		if parsed.usageReport {
			value, err = report.UsageReport(stateDB)
		} else {
			value, err = evidence.ExportRun(stateDB, *parsed.exportRun)
		}
		if err == nil {
			err = printJSON(stdout, value)
		}
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		return 0
	}
	if err := service(parsed, env, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

func service(parsed arguments, env func(string) (string, bool), stdout, stderr io.Writer) error {
	env, err := loadSecretFiles(env, os.Setenv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(parsed.dataDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(parsed.dataDir, 0o700); err != nil {
		return err
	}
	data, err := filepath.EvalSymlinks(parsed.dataDir)
	if err != nil {
		return err
	}
	if data, err = filepath.Abs(data); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(data, "service.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("Another Octomus service is using this data directory")
	}
	state, err := store.Open(filepath.Join(data, stateDBName))
	if err != nil {
		return err
	}
	defer state.Close()
	backend := sandboxBackend(parsed.sandbox, env, stderr)
	startup, stopStartup := signal.NotifyContext(context.Background(), shutdownSignals()...)
	deployment, err := prepareDeployment(startup, parsed.sandbox, data, env, stderr)
	stopStartup()
	if err != nil {
		return err
	}
	app := engine.New(state, data, engine.WithSandbox(backend), engine.WithDeployment(deployment))
	if parsed.doctor {
		mode := model.CycleModeExecution
		if parsed.audit {
			mode = model.CycleModeAudit
		}
		sigCtx, stopSignals := signal.NotifyContext(context.Background(), shutdownSignals()...)
		defer stopSignals()
		return runDoctor(sigCtx, app, mode, stdout, stderr)
	}
	token, ok := env(redact.TokenEnv)
	if !ok {
		return fmt.Errorf("Set %s to a random operator token of at least 32 characters (openssl rand -hex 32)", redact.TokenEnv)
	}
	if len(token) < 32 {
		return fmt.Errorf("%s must contain at least 32 characters", redact.TokenEnv)
	}
	var assetsOverride string
	if parsed.assets != nil {
		if stat, err := os.Stat(filepath.Join(*parsed.assets, "200.html")); err != nil || stat.IsDir() {
			return errors.New("Dashboard override missing 200.html; build the dashboard or correct --assets")
		}
		assetsOverride = *parsed.assets
	}
	if !parsed.listenAddr.Addr().IsLoopback() {
		if _, container := env("OCTOMUS_CONTAINER"); container {
			fmt.Fprintf(stderr, "Non-loopback listener %s inside the container: publish it on 127.0.0.1 only, as the supplied compose file does, and reach it through an SSH tunnel.\n", parsed.listen)
		} else {
			fmt.Fprintf(stderr, "Non-loopback listener %s exposes operator access. Use a loopback address and an SSH tunnel; the token grants full operator control.\n", parsed.listen)
		}
	}
	webhook, _ := env(redact.WebhookEnv)
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stopSignals()
	server := newHTTPServer(httpapi.Router(app, token, assetsOverride, octomus.Version))
	components := serviceComponents{
		scheduler: app,
		http:      server,
		startWorker: func() (func(), error) {
			worker, err := notifications.Start(app.Context(), state, webhook)
			if err != nil || worker == nil {
				return nil, err
			}
			return worker.Stop, nil
		},
	}
	return components.run(sigCtx, parsed.listen, stderr)
}

func shutdownSignals() []os.Signal {
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		signals = append(signals, syscall.SIGHUP)
	}
	return signals
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
}

func runDoctor(ctx context.Context, app *engine.App, mode model.CycleMode, stdout, stderr io.Writer) error {
	shutdownDone := make(chan struct{})
	stopShutdown := context.AfterFunc(ctx, func() {
		defer close(shutdownDone)
		app.Shutdown()
	})
	defer func() {
		if !stopShutdown() {
			<-shutdownDone
		}
	}()
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	result, warnings, err := app.DoctorFor(cfg, mode)
	for _, warning := range warnings {
		fmt.Fprintf(stderr, "WARN %s\n", warning)
	}
	if ctx.Err() != nil {
		return errors.New("Doctor interrupted")
	}
	if err != nil {
		return err
	}
	return printJSON(stdout, result)
}

func parse(args []string, env func(string) (string, bool)) (arguments, string, error) {
	a := arguments{dataDir: ".octomus", listen: "127.0.0.1:4200", sandbox: sandbox.ModeDocker}
	for key, dst := range map[string]*string{"OCTOMUS_DATA_DIR": &a.dataDir, "OCTOMUS_LISTEN": &a.listen} {
		if v, ok := env(key); ok {
			*dst = v
		}
	}
	if v, ok := env("OCTOMUS_SANDBOX"); ok {
		mode, err := sandbox.ParseMode(v)
		if err != nil {
			return a, "", fmt.Errorf("invalid value %q for OCTOMUS_SANDBOX: %w", v, err)
		}
		a.sandbox = mode
	}
	if v, ok := env("OCTOMUS_ASSETS"); ok {
		a.assets = &v
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || strings.HasPrefix(arg, "-h") {
			return a, "help", nil
		}
		if arg == "--version" || strings.HasPrefix(arg, "-V") {
			return a, "version", nil
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if seen[name] {
			return a, "", fmt.Errorf("the argument '%s' cannot be used multiple times", name)
		}
		seen[name] = true
		switch name {
		case "--data-dir", "--listen", "--assets", "--export-run", "--sandbox":
			if !hasValue {
				if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
					return a, "", fmt.Errorf("a value is required for '%s'", name)
				}
				i++
				value = args[i]
			}
			if value == "" && name != "--listen" {
				return a, "", fmt.Errorf("a value is required for '%s'", name)
			}
			switch name {
			case "--data-dir":
				a.dataDir = value
			case "--listen":
				a.listen = value
				if _, err := parseListen(value); err != nil {
					return a, "", err
				}
			case "--assets":
				a.assets = &value
			case "--export-run":
				a.exportRun = &value
			case "--sandbox":
				mode, err := sandbox.ParseMode(value)
				if err != nil {
					return a, "", fmt.Errorf("invalid value %q for '--sandbox': %w", value, err)
				}
				a.sandbox = mode
			}
		case "--print-config", "--doctor", "--audit", "--usage-report", "--sandboxd", "--egress", "--healthcheck":
			if hasValue {
				return a, "", fmt.Errorf("unexpected value for '%s'", name)
			}
			switch name {
			case "--print-config":
				a.printConfig = true
			case "--doctor":
				a.doctor = true
			case "--audit":
				a.audit = true
			case "--usage-report":
				a.usageReport = true
			case "--sandboxd":
				a.sandboxd = true
			case "--egress":
				a.egress = true
			case "--healthcheck":
				a.healthcheck = true
			}
		case "--":
			if i != len(args)-1 {
				return a, "", fmt.Errorf("unexpected argument '%s'", args[i+1])
			}
		default:
			return a, "", fmt.Errorf("unexpected argument '%s'", arg)
		}
	}
	if a.dataDir == "" || (a.assets != nil && *a.assets == "") {
		return a, "", fmt.Errorf("a nonempty path is required")
	}
	listenAddr, err := parseListen(a.listen)
	if err != nil {
		return a, "", err
	}
	a.listenAddr = listenAddr
	if a.audit && !a.doctor {
		return a, "", fmt.Errorf("--audit requires --doctor")
	}
	if a.usageReport && (a.doctor || a.printConfig) {
		return a, "", fmt.Errorf("--usage-report cannot be used with --doctor or --print-config")
	}
	if a.exportRun != nil && (a.doctor || a.printConfig || a.usageReport) {
		return a, "", fmt.Errorf("--export-run cannot be used with --doctor, --print-config or --usage-report")
	}
	modes := 0
	for _, mode := range []bool{a.sandboxd, a.egress, a.healthcheck, a.doctor, a.printConfig, a.usageReport, a.exportRun != nil} {
		if mode {
			modes++
		}
	}
	if (a.sandboxd || a.egress || a.healthcheck) && modes > 1 {
		return a, "", fmt.Errorf("--sandboxd, --egress and --healthcheck cannot be combined with another mode")
	}
	return a, "", nil
}

func parseListen(listen string) (netip.AddrPort, error) {
	address, err := netip.ParseAddrPort(listen)
	if err == nil && address.Addr().Zone() != "" {
		_, err = strconv.ParseUint(address.Addr().Zone(), 10, 32)
	}
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("invalid value %q for '--listen': invalid socket address syntax", listen)
	}
	return address, nil
}

const help = `Continuous repository improvement through reviewed pull requests

Usage: octomus-agent [OPTIONS]

Options:
      --data-dir <DATA_DIR>     [env: OCTOMUS_DATA_DIR] [default: .octomus]
      --listen <LISTEN>         [env: OCTOMUS_LISTEN] [default: 127.0.0.1:4200]
      --assets <ASSETS>         Override the embedded dashboard with a build directory [env: OCTOMUS_ASSETS]
      --print-config           Print configuration defaults and exit
      --doctor                 Validate saved repository, authentication and model routes, then exit
      --audit                  Check only audit prerequisites with --doctor
      --usage-report           Export a read-only JSON usage report from saved state and exit
      --export-run <CYCLE_ID>   Export read-only JSON run evidence for one saved cycle and exit
      --sandbox <MODE>          Where runners and verification run: docker, through the sandbox broker, or off,
                                directly on this host [env: OCTOMUS_SANDBOX] [default: docker]
      --sandboxd                Serve the sandbox broker (the only component that uses the Docker socket)
      --egress                  Serve the egress gateway that sandboxes reach the internet through
      --healthcheck             Exit 0 when the service on --listen answers its health check
  -h, --help                   Print help
  -V, --version                Print version
`
