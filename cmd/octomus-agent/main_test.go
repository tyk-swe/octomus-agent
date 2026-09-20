package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFrozenCLIContracts(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/compatibility/cli.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Name     string
			Args     []string
			Env      map[string]string
			Expected struct {
				Code           int
				Stdout, Stderr string
			}
		}
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(c.Args, func(k string) (string, bool) { v, ok := c.Env[k]; return v, ok }, &stdout, &stderr)
			if code != c.Expected.Code {
				t.Fatalf("exit %d != %d: %s", code, c.Expected.Code, stderr.String())
			}
			if code != 0 {
				if stdout.Len() != 0 || stderr.Len() == 0 {
					t.Fatal("error stream boundaries")
				}
				return
			}
			if stderr.Len() != 0 {
				t.Fatal(stderr.String())
			}
			if strings.Contains(c.Expected.Stdout, "Usage: octomus-agent [OPTIONS]") {
				for _, flag := range []string{"data-dir", "listen", "assets", "print-config", "doctor", "audit", "usage-report", "export-run", "help", "version"} {
					if !strings.Contains(stdout.String(), "--"+flag) {
						t.Errorf("help omitted --%s", flag)
					}
				}
			} else if stdout.String() != c.Expected.Stdout {
				t.Errorf("stdout differs: %s", stdout.String())
			}
		})
	}
}
func TestUnavailableCommandsFailWithoutSideEffects(t *testing.T) {
	directory := t.TempDir() + "/must-not-exist"
	for _, args := range [][]string{{}, {"--doctor"}, {"--doctor", "--audit"}, {"--usage-report"}, {"--export-run", "cycle"}} {
		var out, err bytes.Buffer
		code := run(append([]string{"--data-dir", directory}, args...), func(string) (string, bool) { return "", false }, &out, &err)
		if code != 1 || out.Len() != 0 || !bytes.Contains(err.Bytes(), []byte("not implemented")) {
			t.Fatal(args, code, out.String(), err.String())
		}
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("unavailable command created state", err)
	}
}
