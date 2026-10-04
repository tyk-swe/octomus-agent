package runner

// workerPolicy is the OpenCode configuration every Octomus server starts with: no sharing, updates, snapshots, helpers
// or compaction, and one primary agent that may do anything but ask questions or delegate.
func workerPolicy(agent string) map[string]any {
	return map[string]any{
		"share":         "disabled",
		"autoshare":     false,
		"autoupdate":    false,
		"snapshot":      false,
		"lsp":           false,
		"formatter":     false,
		"compaction":    map[string]any{"auto": false, "prune": false},
		"default_agent": agent,
		"agent": map[string]any{
			agent: map[string]any{
				"mode":       "primary",
				"prompt":     WorkerInstructions,
				"permission": map[string]any{"*": "allow", "question": "deny", "task": "deny"},
			},
			"title":      map[string]any{"disable": true},
			"summary":    map[string]any{"disable": true},
			"compaction": map[string]any{"disable": true},
		},
	}
}

// appliedPolicy checks that the server's effective configuration is the worker policy.
func appliedPolicy(effective any, agent string) bool {
	doc, ok := asObject(effective)
	if !ok {
		return false
	}
	compaction, _ := asObject(doc["compaction"])
	if doc["share"] != "disabled" || doc["autoshare"] != false || doc["autoupdate"] != false ||
		doc["snapshot"] != false || doc["lsp"] != false || doc["formatter"] != false ||
		compaction["auto"] != false || compaction["prune"] != false || doc["default_agent"] != agent {
		return false
	}
	agents, _ := asObject(doc["agent"])
	worker, _ := asObject(agents[agent])
	permission, _ := asObject(worker["permission"])
	if worker["mode"] != "primary" || worker["prompt"] != WorkerInstructions ||
		permission["*"] != "allow" || permission["question"] != "deny" || permission["task"] != "deny" {
		return false
	}
	for _, helper := range []string{"title", "summary", "compaction"} {
		entry, _ := asObject(agents[helper])
		if entry["disable"] != true {
			return false
		}
	}
	return true
}
