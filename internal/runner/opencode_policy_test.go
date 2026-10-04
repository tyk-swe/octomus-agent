// The applied-policy check that refuses an OpenCode server whose effective config drifted from the worker policy.

package runner

import (
	"testing"
)

func TestAppliedPolicyRejectsEachDrift(t *testing.T) {
	t.Parallel()
	const agent = "octomus-x"
	encoded, err := marshal(workerPolicy(agent))
	if err != nil {
		t.Fatal(err)
	}
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
