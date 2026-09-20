package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/jsoncompat"
	dashboard "github.com/tyk-swe/octomus-agent/web"
)

const version = "0.1.0"

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
		fmt.Fprintln(stdout, "octomus-agent "+version)
		return 0
	}
	if parsed.printConfig {
		data, err := jsoncompat.Marshal(config.Default())
		if err == nil {
			var pretty bytes.Buffer
			err = json.Indent(&pretty, data, "", "  ")
			if err == nil {
				_, err = fmt.Fprintln(stdout, pretty.String())
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		return 0
	}
	// Linking the real filesystem keeps all dashboard bytes in the executable.
	// Serving it, operator authentication and durable startup belong to M7.
	if _, err := fs.Stat(dashboard.Files(), "200.html"); err != nil {
		fmt.Fprintf(stderr, "Error: embedded dashboard: %v\n", err)
		return 1
	}
	command, milestone := "service startup", "M7"
	switch {
	case parsed.usageReport:
		command, milestone = "--usage-report", "M2"
	case parsed.exportRun != nil:
		command, milestone = "--export-run", "M2"
	case parsed.doctor:
		command = "--doctor"
	}
	fmt.Fprintf(stderr, "Error: %s is not implemented in the Go executable yet (requires %s)\n", command, milestone)
	return 1
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
		if arg == "--help" || arg == "-h" {
			return a, "help", nil
		}
		if arg == "--version" || arg == "-V" {
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
		// Rust SocketAddr accepts decimal u32 scope IDs, but not interface names.
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
