package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

type brokerReporter interface {
	Info(context.Context) (wire.BrokerInfo, error)
	RefreshInfo(context.Context) (wire.BrokerInfo, error)
}

// A broker process has immutable deployment settings. Its fresh identity also invalidates proof after changes
// that its display fields cannot describe, such as helper binaries, mount sources, user IDs or host restarts.
func postureFingerprint(info wire.BrokerInfo, deployment Deployment) (string, error) {
	if info.InstanceID == "" || info.ImageID == "" {
		return "", errors.New("Sandbox broker does not report its process and image identity; update the broker")
	}
	if info.Egress && (info.Gateway == nil || info.Gateway.InstanceID == "" || info.Gateway.PolicyFingerprint == "") {
		return "", errors.New("Sandbox gateway identity and policy are unavailable; restore the egress collector before running the self-test")
	}
	hosts := make(map[string][]string, len(deployment.Egress))
	for kind, names := range deployment.Egress {
		copy := slices.Clone(names)
		slices.Sort(copy)
		hosts[kind] = slices.Compact(copy)
	}
	// Live counts and version-probe diagnostics vary independently of the isolation boundary.
	data, err := json.Marshal(struct {
		Instance, Version, Docker, API, Image, Runtime string
		Limits                                         wire.BrokerLimits
		Networks                                       wire.BrokerNetworks
		Egress                                         bool
		Gateway                                        *wire.GatewayPosture
		Hosts                                          map[string][]string
		Repository, Checkout                           string
	}{info.InstanceID, info.Version, info.DockerVersion, info.APIVersion, info.ImageID, info.Runtime,
		info.Limits, info.Networks, info.Egress, info.Gateway, hosts, deployment.GitHubRepo, deployment.Repository})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (r SandboxSelfTest) matchesPosture(info wire.BrokerInfo, deployment Deployment) bool {
	if r.PostureFingerprint == nil || r.ImageID != info.ImageID || r.Runtime != info.Runtime {
		return false
	}
	fingerprint, err := postureFingerprint(info, deployment)
	return err == nil && fingerprint == *r.PostureFingerprint
}

func (r *SandboxSelfTest) bindPosture(before, after wire.BrokerInfo, deployment Deployment) error {
	// Image resolution can legitimately refresh at probe admission. Its exit evidence is the authoritative image.
	before.ImageID = r.ImageID
	if r.ImageID != after.ImageID || r.Runtime != after.Runtime {
		return errors.New("Sandbox image changed during the self-test; run it again")
	}
	first, err := postureFingerprint(before, deployment)
	if err != nil {
		return err
	}
	last, err := postureFingerprint(after, deployment)
	if err != nil {
		return err
	}
	if first != last {
		return errors.New("Sandbox posture changed during the self-test; run it again")
	}
	r.PostureFingerprint = &last
	return nil
}
