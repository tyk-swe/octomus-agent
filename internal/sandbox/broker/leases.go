package broker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// leases are the egress gateway's record of which proxy credential belongs to which live sandbox.
type leases struct{ dir string }

func (l *leases) grant(name string, kind sandbox.Kind) (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(secret[:])
	data, err := json.Marshal(sandbox.Lease{Sandbox: name, Kind: kind.String()})
	if err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(l.dir, ".lease-*")
	if err != nil {
		return "", err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(temp.Name())
		return "", err
	}
	if err := temp.Close(); err != nil {
		os.Remove(temp.Name())
		return "", err
	}
	if err := os.Rename(temp.Name(), filepath.Join(l.dir, sandbox.LeaseFile(token))); err != nil {
		os.Remove(temp.Name())
		return "", err
	}
	return token, nil
}

func (l *leases) revoke(token string) {
	_ = os.Remove(filepath.Join(l.dir, sandbox.LeaseFile(token)))
}

func (l *leases) clear() error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".lease-") {
			if err := os.Remove(filepath.Join(l.dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// proxyEnv points every common proxy variable at the egress gateway with this sandbox's credential. Loopback stays
// direct so local test servers and the OpenCode bridge keep working.
func proxyEnv(proxy, token string) []string {
	address := "http://" + sandbox.ProxyUser + ":" + token + "@" + proxy
	env := []string{}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		env = append(env, name+"="+address, strings.ToLower(name)+"="+address)
	}
	return append(env, "NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1")
}
