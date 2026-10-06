package egress

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Lease tells the gateway which sandbox holds a proxy credential and which allowlist applies to it. The broker grants
// a lease before a sandbox starts and revokes it once the container is gone; the gateway only reads them. Runner
// logins get one too, from the --login-lease mode that deploy/docker/compose.yaml's login-lease service runs; it lasts
// until the next login or a broker restart.
type Lease struct {
	Sandbox string `json:"sandbox"`
	Kind    string `json:"kind"`
}

// proxyUser is the fixed user name in every sandbox's proxy credential; the password identifies the sandbox.
const proxyUser = "sandbox"

const (
	// tokenBytes is the entropy of a proxy credential, which is hex-encoded.
	tokenBytes = 32
	// tempPrefix marks a lease still being written.
	tempPrefix = ".lease-"
	// maxLeaseBytes bounds the lease file the gateway reads.
	maxLeaseBytes = 4096
)

// Leases is the directory of leases: the record of which proxy credential belongs to which live sandbox.
type Leases struct{ Dir string }

// leaseFile names a lease by the digest of its proxy credential, so the credential itself is never stored.
func leaseFile(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]) + ".json"
}

// Grant writes a lease for a sandbox of this kind and returns its new proxy credential.
func (l Leases) Grant(sandbox, kind string) (string, error) {
	var secret [tokenBytes]byte
	_, _ = rand.Read(secret[:])
	token := hex.EncodeToString(secret[:])
	data, err := json.Marshal(Lease{Sandbox: sandbox, Kind: kind})
	if err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(l.Dir, tempPrefix+"*")
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
	if err := os.Rename(temp.Name(), filepath.Join(l.Dir, leaseFile(token))); err != nil {
		os.Remove(temp.Name())
		return "", err
	}
	return token, nil
}

// Revoke deletes the lease of a proxy credential; the gateway ends its tunnels at its next sweep.
func (l Leases) Revoke(token string) {
	_ = os.Remove(filepath.Join(l.Dir, leaseFile(token)))
}

// Clear deletes every lease, and any lease left half written.
func (l Leases) Clear() error {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), tempPrefix) {
			if err := os.Remove(filepath.Join(l.Dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// lookup returns the live lease of a proxy credential and the name of its file.
func (l Leases) lookup(token string) (Lease, string, bool) {
	if len(token) != 2*tokenBytes {
		return Lease{}, "", false
	}
	if _, err := hex.DecodeString(token); err != nil {
		return Lease{}, "", false
	}
	file := leaseFile(token)
	data, err := os.ReadFile(filepath.Join(l.Dir, file))
	if err != nil || len(data) > maxLeaseBytes {
		return Lease{}, "", false
	}
	var lease Lease
	if json.Unmarshal(data, &lease) != nil || lease.Sandbox == "" {
		return Lease{}, "", false
	}
	return lease, file, true
}

// revoked reports whether the lease named file is gone.
func (l Leases) revoked(file string) bool {
	_, err := os.Stat(filepath.Join(l.Dir, file))
	return errors.Is(err, os.ErrNotExist)
}

// ProxyURL is the gateway's address with a sandbox's proxy credential.
func ProxyURL(proxy, token string) string {
	return "http://" + proxyUser + ":" + token + "@" + proxy
}

// ProxyEnv points every common proxy variable at the egress gateway with this sandbox's credential. Loopback stays
// direct so local test servers and the OpenCode bridge keep working.
func ProxyEnv(proxy, token string) []string {
	address := ProxyURL(proxy, token)
	env := []string{}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		env = append(env, name+"="+address, strings.ToLower(name)+"="+address)
	}
	return append(env, "NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1")
}
