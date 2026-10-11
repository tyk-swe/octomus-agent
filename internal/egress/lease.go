package egress

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/wirejson"
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

// Revoke confirms that a proxy credential's lease is absent; the gateway ends its tunnels at its next sweep.
// Its owner must retain failed revocations for retry. Diagnostics name only the digest file, never the credential.
func (l Leases) Revoke(token string) error {
	err := os.Remove(filepath.Join(l.Dir, leaseFile(token)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RevokeLogin confirms that every login authorization is gone before its replacement is granted. Lease metadata
// owns that identity, including a grant interrupted before its proxy file was written; unrelated leases survive.
// Unreadable or malformed metadata cannot prove it belongs to another sandbox, so it prevents a new login grant.
func (l Leases) RevokeLogin() error {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file := filepath.Join(l.Dir, entry.Name())
		lease, err := readLease(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("Inspecting egress lease metadata %s: %w", entry.Name(), err))
			continue
		}
		if lease.Sandbox == "login" {
			if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
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
	lease, err := readLease(filepath.Join(l.Dir, file))
	if err != nil {
		return Lease{}, "", false
	}
	return lease, file, true
}

func readLease(file string) (Lease, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return Lease{}, err
	}
	if !info.Mode().IsRegular() {
		return Lease{}, errors.New("Egress lease metadata is not a regular file")
	}
	input, err := os.Open(file)
	if err != nil {
		return Lease{}, err
	}
	defer input.Close()
	data, err := io.ReadAll(io.LimitReader(input, maxLeaseBytes+1))
	if err != nil {
		return Lease{}, err
	}
	if len(data) > maxLeaseBytes {
		return Lease{}, errors.New("Egress lease metadata exceeds its size limit")
	}
	var lease Lease
	if err := wirejson.DecodeRecord(data, &lease); err != nil {
		return Lease{}, err
	}
	if lease.Sandbox == "" || lease.Kind == "" {
		return Lease{}, errors.New("Egress lease metadata is incomplete")
	}
	return lease, nil
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
