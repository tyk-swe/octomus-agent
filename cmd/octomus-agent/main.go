package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/tyk-swe/octomus-agent/internal/report"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	dashboard "github.com/tyk-swe/octomus-agent/web"
)

// stateDBName is the SQLite file inside the data directory.
const stateDBName = "state.db"

// printJSON writes two-space indented JSON with sorted object keys and a trailing newline.
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
	dataDir, listen                         string
	assets, exportRun                       *string
	printConfig, doctor, audit, usageReport bool
}

func main() { os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr)) }
func run(args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
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
	// Read-only exports run before directory creation, service locking or worker
	// startup, and need no operator token.
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
	// Linking the real filesystem keeps all dashboard bytes in the executable.
	if _, err := fs.Stat(dashboard.Files(), "200.html"); err != nil {
		fmt.Fprintf(stderr, "Error: embedded dashboard: %v\n", err)
		return 1
	}
	if err := service(parsed, env, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

// service owns startup: data directory, process lock, store,
// optional doctor, token, assets, workers, listener and graceful shutdown.
func service(parsed arguments, env func(string) (string, bool), stdout, stderr io.Writer) error {
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
	app := engine.New(state, data)
	if parsed.doctor {
		mode := model.CycleModeExecution
		if parsed.audit {
			mode = model.CycleModeAudit
		}
		sigCtx, stopSignals := signal.NotifyContext(context.Background(), shutdownSignals()...)
		defer stopSignals()
		return runDoctor(sigCtx, app, mode, stdout)
	}
	token, ok := env(httpapi.TokenEnv)
	if !ok {
		return fmt.Errorf("Set %s to a random operator token of at least 32 characters (openssl rand -hex 32)", httpapi.TokenEnv)
	}
	if len(token) < 32 {
		return fmt.Errorf("%s must contain at least 32 characters", httpapi.TokenEnv)
	}
	var assetsOverride string
	if parsed.assets != nil {
		if stat, err := os.Stat(filepath.Join(*parsed.assets, "200.html")); err != nil || stat.IsDir() {
			return errors.New("Dashboard override missing 200.html; build the dashboard or correct --assets")
		}
		assetsOverride = *parsed.assets
	}
	listen, err := netip.ParseAddrPort(parsed.listen)
	if err != nil {
		return fmt.Errorf("invalid value %q for '--listen': invalid socket address syntax", parsed.listen)
	}
	if !listen.Addr().IsLoopback() {
		fmt.Fprintf(stderr, "Non-loopback listener %s exposes operator access. Use a loopback address and an SSH tunnel; the token grants full operator control.\n", parsed.listen)
	}
	webhook, _ := env(store.WebhookEnv)
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

// shutdownSignals are the signals that stop the service, or an interrupted
// doctor, gracefully. A hangup (a closed terminal or dropped SSH session) joins
// them so owned process groups are terminated instead of orphaned, unless the
// hangup is already ignored, as under nohup: registering it would un-ignore it.
func shutdownSignals() []os.Signal {
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		signals = append(signals, syscall.SIGHUP)
	}
	return signals
}

// newHTTPServer bounds only the connection phases no handler needs: headers
// must arrive within ReadHeaderTimeout and an idle keep-alive connection
// closes after IdleTimeout, so stalled or abandoned clients cannot pin
// descriptors. Read and write stay unbounded because doctor and model catalog
// requests legitimately run for about a minute.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
}

// runDoctor validates the saved configuration for mode and prints the result.
// Cancelling ctx (one of the shutdownSignals) shuts the app down,
// which terminates every owned process group the checks started, and fails
// the command. The shutdown finishes before runDoctor returns, so the caller
// may close the store.
func runDoctor(ctx context.Context, app *engine.App, mode model.CycleMode, stdout io.Writer) error {
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
	result, err := app.DoctorFor(cfg, mode)
	if ctx.Err() != nil {
		return errors.New("Doctor interrupted")
	}
	if err != nil {
		return err
	}
	return printJSON(stdout, result)
}

func parse(args []string, env func(string) (string, bool)) (arguments, string, error) {
	a := arguments{dataDir: ".octomus", listen: "127.0.0.1:4200"}
	for key, dst := range map[string]*string{"OCTOMUS_DATA_DIR": &a.dataDir, "OCTOMUS_LISTEN": &a.listen} {
		if v, ok := env(key); ok {
			*dst = v
		}
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
		case "--data-dir", "--listen", "--assets", "--export-run":
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
				if err := validateListen(value); err != nil {
					return a, "", err
				}
			case "--assets":
				a.assets = &value
			case "--export-run":
				a.exportRun = &value
			}
		case "--print-config", "--doctor", "--audit", "--usage-report":
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
	if err := validateListen(a.listen); err != nil {
		return a, "", err
	}
	if a.audit && !a.doctor {
		return a, "", fmt.Errorf("--audit requires --doctor")
	}
	if a.usageReport && (a.doctor || a.printConfig) {
		return a, "", fmt.Errorf("--usage-report cannot be used with --doctor or --print-config")
	}
	if a.exportRun != nil && (a.doctor || a.printConfig || a.usageReport) {
		return a, "", fmt.Errorf("--export-run cannot be used with --doctor, --print-config or --usage-report")
	}
	return a, "", nil
}

func validateListen(listen string) error {
	address, err := netip.ParseAddrPort(listen)
	if err == nil && address.Addr().Zone() != "" {
		// Scope IDs must be decimal 32-bit numbers, rather than interface names.
		_, err = strconv.ParseUint(address.Addr().Zone(), 10, 32)
	}
	if err != nil {
		return fmt.Errorf("invalid value %q for '--listen': invalid socket address syntax", listen)
	}
	return nil
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
  -h, --help                   Print help
  -V, --version                Print version
`
