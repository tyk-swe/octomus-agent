package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
)

// Lease tells the egress gateway which sandbox holds a proxy credential and which allowlist applies to it. The broker
// writes a lease before a sandbox starts and deletes it once the container is gone; the gateway only reads them.
// deploy/docker/compose.yaml's login-lease service also writes this format for runner logins (tests/docker_setup.py
// checks it); that lease lasts until the next login or a broker restart.
type Lease struct {
	Sandbox string `json:"sandbox"`
	Kind    string `json:"kind"`
}

// LeaseFile names a lease by the digest of its proxy credential, so the credential itself is never stored.
func LeaseFile(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]) + ".json"
}

// ProxyUser is the fixed user name in every sandbox's proxy credential; the password identifies the sandbox.
const ProxyUser = "sandbox"
