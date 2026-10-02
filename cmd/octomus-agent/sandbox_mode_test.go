package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

func sandboxEnvironment(value string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key == "OCTOMUS_SANDBOX" {
			return value, true
		}
		return "", false
	}
}

func TestSandboxFlagOverridesEnvironmentDefault(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		args []string
		env  string
		want sandbox.Mode
	}{
		{nil, "docker", sandbox.ModeDocker},
		{nil, "off", sandbox.ModeOff},
		{[]string{"--sandbox", "docker"}, "off", sandbox.ModeDocker},
		{[]string{"--sandbox=off"}, "docker", sandbox.ModeOff},
		{[]string{"--sandbox", "docker"}, "mistyped", sandbox.ModeDocker},
		{[]string{"--sandbox=off"}, "mistyped", sandbox.ModeOff},
		{[]string{"--sandbox=off"}, "", sandbox.ModeOff},
	} {
		parsed, display, err := parse(test.args, sandboxEnvironment(test.env))
		if err != nil || display != "" || parsed.sandbox != test.want {
			t.Fatalf("%v with OCTOMUS_SANDBOX=%q: mode=%v display=%q err=%v", test.args, test.env, parsed.sandbox, display, err)
		}
	}
}

func TestHelpAndVersionIgnoreInvalidEnvironmentWithoutState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, arg := range []string{"--help", "-h", "--version", "-V"} {
		var out, stderr bytes.Buffer
		code := run([]string{arg}, func(key string) (string, bool) {
			switch key {
			case "OCTOMUS_SANDBOX", "OCTOMUS_LISTEN":
				return "mistyped", true
			case "OCTOMUS_DATA_DIR":
				return filepath.Join(root, "must-not-exist"), true
			default:
				return "", false
			}
		}, &out, &stderr)
		if code != 0 || stderr.Len() != 0 || out.Len() == 0 {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", arg, code, out.String(), stderr.String())
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("display commands wrote state: %v, %v", entries, err)
	}
}

func TestInvalidSandboxStillRefusesActualCommands(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, value := range []string{"mistyped", ""} {
		for _, command := range [][]string{nil, {"--doctor"}, {"--print-config"}, {"--usage-report"}, {"--export-run", "fixture"}, {"--healthcheck"}, {"--sandboxd"}, {"--sandboxd-check"}, {"--egress"}} {
			args := append([]string{"--data-dir", filepath.Join(root, "must-not-exist")}, command...)
			var out, stderr bytes.Buffer
			if code := run(args, sandboxEnvironment(value), &out, &stderr); code != 2 || out.Len() != 0 || !strings.Contains(stderr.String(), "for OCTOMUS_SANDBOX") {
				t.Fatalf("%v with OCTOMUS_SANDBOX=%q: code=%d stdout=%q stderr=%q", args, value, code, out.String(), stderr.String())
			}
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("invalid environment wrote state: %v, %v", entries, err)
	}
	for _, args := range [][]string{{"--sandbox", "mistyped"}, {"--sandbox=mistyped", "--help"}, {"--sandbox=mistyped", "--version"}} {
		_, _, err := parse(args, sandboxEnvironment("docker"))
		if err == nil || !strings.Contains(err.Error(), `invalid value "mistyped" for '--sandbox'`) {
			t.Fatalf("%v accepted an invalid explicit flag: %v", args, err)
		}
	}
}
