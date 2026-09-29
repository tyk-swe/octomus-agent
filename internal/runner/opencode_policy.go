package runner

import (
	"crypto/rand"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

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

var parseReadyURL = sandbox.ParseLoopbackURL

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

var messageClock atomic.Uint64

func messageID() (string, error) {
	var random [14]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return "", err
	}
	now := uint64(time.Now().UnixMilli()) * 4096
	for {
		previous := messageClock.Load()
		next := previous + 1
		if next < now+1 {
			next = now + 1
		}
		if messageClock.CompareAndSwap(previous, next) {
			clock := next & 0xffff_ffff_ffff
			const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
			suffix := make([]byte, 14)
			for i := range suffix {
				suffix[i] = alphabet[int(random[i])%len(alphabet)]
			}
			return fmt.Sprintf("msg_%012x%s", clock, suffix), nil
		}
	}
}

func variantMatches(reported string, ok bool, route config.Route) bool {
	if route.Variant != nil {
		return ok && reported == *route.Variant
	}
	return !ok || reported == "default"
}

func checkModel(info map[string]any, route config.Route) error {
	modelID, _ := strAt(info, "modelID")
	providerID, providerOK := strAt(info, "providerID")
	if modelID != route.Model || providerOK != (route.Provider != nil) || (providerOK && providerID != *route.Provider) {
		return fmt.Errorf("OpenCode substituted the requested model")
	}
	variant, variantOK := strAt(info, "variant")
	if !variantMatches(variant, variantOK, route) {
		return fmt.Errorf("OpenCode substituted the requested variant")
	}
	return nil
}

func segment(id string) (string, error) {
	valid := id != "" && len(id) <= 256
	if valid {
		for i := 0; i < len(id); i++ {
			b := id[i]
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-') {
				valid = false
				break
			}
		}
	}
	if !valid {
		return "", fmt.Errorf("Invalid OpenCode identity")
	}
	var encoded strings.Builder
	for i := 0; i < len(id); i++ {
		b := id[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
			encoded.WriteByte(b)
		} else {
			fmt.Fprintf(&encoded, "%%%02X", b)
		}
	}
	return encoded.String(), nil
}
