package runner

import (
	"strings"
	"testing"
)

// The owned server must report a loopback root address: plain http, the
// literal 127.0.0.1, a nonzero explicit port, and no credentials, path,
// query or fragment. Anything else is refused rather than normalized.
func TestParseReadyURL(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1:4096", "http://127.0.0.1:4096/"} {
		got, err := parseReadyURL(endpoint)
		if err != nil || got != "http://127.0.0.1:4096" {
			t.Errorf("parseReadyURL(%q) = %q, %v", endpoint, got, err)
		}
	}
	for _, endpoint := range []string{
		"",
		"127.0.0.1:4096",
		"https://127.0.0.1:4096/",
		"ws://127.0.0.1:4096/",
		"http://localhost:4096/",
		"http://10.0.0.1:4096/",
		"http://[::1]:4096/",
		"http://127.0.0.1/",
		"http://127.0.0.1:0/",
		"http://127.0.0.1:65536/",
		// The scheme's default port is elided by URL parsing, so it is not
		// an explicit port.
		"http://127.0.0.1:80/",
		"http://user@127.0.0.1:4096/",
		"http://user:pw@127.0.0.1:4096/",
		"http://@127.0.0.1:4096/",
		"http://127.0.0.1:4096/x",
		"http://127.0.0.1:4096/?q",
		"http://127.0.0.1:4096?q",
		"http://127.0.0.1:4096/#f",
		"http://127.0.0.1:4096#f",
	} {
		if got, err := parseReadyURL(endpoint); err == nil || got != "" {
			t.Errorf("parseReadyURL(%q) = %q, %v; want a refusal", endpoint, got, err)
		}
	}
}

// segment admits only [A-Za-z0-9_-] identities of 1-256 bytes and encodes
// every byte that is not an ASCII letter or digit, so a runner-supplied ID is
// always exactly one path segment.
func TestSegment(t *testing.T) {
	for id, want := range map[string]string{
		"ses_ab-C9":              "ses%5Fab%2DC9",
		"AZaz09":                 "AZaz09",
		"-":                      "%2D",
		strings.Repeat("a", 256): strings.Repeat("a", 256),
	} {
		if got, err := segment(id); err != nil || got != want {
			t.Errorf("segment(%q) = %q, %v; want %q", id, got, err, want)
		}
	}
	for _, id := range []string{
		"",
		strings.Repeat("a", 257),
		"a/b",
		".",
		"..",
		"a.b",
		"a b",
		"a%2F",
		"a?b",
		"a#b",
		"a\\b",
		"a\x00b",
		"\u00e9",
	} {
		if got, err := segment(id); err == nil || err.Error() != "Invalid OpenCode identity" || got != "" {
			t.Errorf("segment(%q) = %q, %v; want a refusal", id, got, err)
		}
	}
}

// The policy sent to the server is exactly what the effective-config check
// accepts once it round-trips through JSON, and only for its own agent.
func TestWorkerPolicyPassesAppliedPolicy(t *testing.T) {
	encoded, err := marshal(workerPolicy("octomus-a"))
	if err != nil {
		t.Fatal(err)
	}
	effective, err := decodeJSON([]byte(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if !appliedPolicy(effective, "octomus-a") {
		t.Fatalf("the worker policy must pass its own check: %s", encoded)
	}
	if appliedPolicy(effective, "octomus-b") {
		t.Fatal("another agent's policy must not pass")
	}
}

// Managed host settings can override any part of the inline policy. Each
// single safety-critical drift from the policy the adapter sends makes the
// effective config fail the check; unrelated extra settings do not.
func TestAppliedPolicyRejectsEachDrift(t *testing.T) {
	const agent = "octomus-x"
	encoded, err := marshal(workerPolicy(agent))
	if err != nil {
		t.Fatal(err)
	}
	// effective decodes a fresh copy of the sent policy, as the server
	// reports it, so each case mutates its own document.
	effective := func() map[string]any {
		t.Helper()
		value, err := decodeJSON([]byte(encoded))
		if err != nil {
			t.Fatal(err)
		}
		return value.(map[string]any)
	}
	object := func(doc map[string]any, path ...string) map[string]any {
		t.Helper()
		for _, key := range path {
			next, ok := doc[key].(map[string]any)
			if !ok {
				t.Fatalf("policy has no object at %v", path)
			}
			doc = next
		}
		return doc
	}
	extra := effective()
	extra["theme"] = "dark"
	object(extra, "agent")["build"] = map[string]any{"mode": "primary"}
	if !appliedPolicy(extra, agent) {
		t.Fatal("settings the policy does not name must not fail the check")
	}
	for name, drift := range map[string]func(map[string]any){
		"share auto":                 func(d map[string]any) { d["share"] = "auto" },
		"autoshare on":               func(d map[string]any) { d["autoshare"] = true },
		"autoshare as a string":      func(d map[string]any) { d["autoshare"] = "false" },
		"autoupdate unset":           func(d map[string]any) { delete(d, "autoupdate") },
		"snapshot on":                func(d map[string]any) { d["snapshot"] = true },
		"lsp on":                     func(d map[string]any) { d["lsp"] = true },
		"formatter on":               func(d map[string]any) { d["formatter"] = true },
		"compaction auto":            func(d map[string]any) { object(d, "compaction")["auto"] = true },
		"compaction prune":           func(d map[string]any) { object(d, "compaction")["prune"] = true },
		"compaction unset":           func(d map[string]any) { delete(d, "compaction") },
		"another default agent":      func(d map[string]any) { d["default_agent"] = "build" },
		"worker as a subagent":       func(d map[string]any) { object(d, "agent", agent)["mode"] = "subagent" },
		"worker prompt replaced":     func(d map[string]any) { object(d, "agent", agent)["prompt"] = "x" },
		"worker unset":               func(d map[string]any) { delete(object(d, "agent"), agent) },
		"permissions ask":            func(d map[string]any) { object(d, "agent", agent, "permission")["*"] = "ask" },
		"question allowed":           func(d map[string]any) { object(d, "agent", agent, "permission")["question"] = "allow" },
		"task allowed":               func(d map[string]any) { object(d, "agent", agent, "permission")["task"] = "allow" },
		"permissions unset":          func(d map[string]any) { delete(object(d, "agent", agent), "permission") },
		"title helper enabled":       func(d map[string]any) { object(d, "agent", "title")["disable"] = false },
		"summary helper unset":       func(d map[string]any) { delete(object(d, "agent"), "summary") },
		"compaction helper reset":    func(d map[string]any) { object(d, "agent")["compaction"] = map[string]any{} },
		"helper disable as a string": func(d map[string]any) { object(d, "agent", "compaction")["disable"] = "true" },
	} {
		doc := effective()
		drift(doc)
		if appliedPolicy(doc, agent) {
			t.Errorf("%s: a drifted policy must fail the check", name)
		}
	}
	for _, value := range []any{nil, []any{}, "policy"} {
		if appliedPolicy(value, agent) {
			t.Errorf("a %T effective config must fail the check", value)
		}
	}
}
