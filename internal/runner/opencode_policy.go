package runner

// The owned OpenCode server's unattended policy and the checks on what it
// reports: its ready address, effective config, identities and model route.
import (
	"crypto/rand"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	whatwg "github.com/nlnwa/whatwg-url/url"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

// workerPolicy is the unattended inline config the owned server runs under:
// sharing, updates, snapshots, LSP, formatting and compaction are off; agent
// is the default primary agent with the worker instructions and every
// permission except question and task; the helper agents are disabled.
// appliedPolicy checks the server's effective config against it.
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

// parseReadyURL accepts only a loopback root address: http scheme, literal
// 127.0.0.1, a nonzero explicit port, root path, and no user, query, or
// fragment.
func parseReadyURL(endpoint string) (string, error) {
	u, err := whatwg.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("Invalid OpenCode server address: %w", err)
	}
	port, perr := strconv.Atoi(u.Port())
	authority := endpoint
	if i := strings.Index(authority, "://"); i >= 0 {
		authority = authority[i+3:]
	}
	if i := strings.IndexByte(authority, '/'); i >= 0 {
		authority = authority[:i]
	}
	if u.Scheme() != "http" || u.Hostname() != "127.0.0.1" || perr != nil || port <= 0 ||
		u.Username() != "" || u.Password() != "" || strings.Contains(authority, "@") ||
		u.Pathname() != "/" || strings.ContainsAny(endpoint, "?#") {
		return "", fmt.Errorf("OpenCode did not bind to a local server address")
	}
	return fmt.Sprintf("http://127.0.0.1:%s", u.Port()), nil
}

// appliedPolicy validates every safety-critical field of the effective config,
// beyond the expected fields.
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

// messageID generates a native ordered 30-char msg_ ID: 12 lower hex chars
// from the low 48 bits of a monotonically increasing
// (milliseconds*4096+counter) clock, then 14 base62 random chars. Entropy
// failure is never silently ignored.
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

// variantMatches accepts the exact variant, or `default` when the route has
// none.
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

// segment validates an OpenCode identity and percent-encodes it like
// NON_ALPHANUMERIC.
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

// jsonEqual compares two decoded JSON values by canonical compact form, like
// JSON value equality.
func jsonEqual(a, b any) bool {
	ea, err1 := marshal(a)
	eb, err2 := marshal(b)
	return err1 == nil && err2 == nil && ea == eb
}
