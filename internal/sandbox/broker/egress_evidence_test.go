package broker

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestPendingEgressMarksEvidenceIncompleteWithoutLosingKnownCounts(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, alreadyIncomplete := range []bool{false, true} {
			t.Run(fmt.Sprintf("pending=%v/incomplete=%v", pending, alreadyIncomplete), func(t *testing.T) {
				log := &testutil.SyncBuffer{}
				started := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
				s := &prepared{b: &Broker{cfg: Config{Log: log}}, name: "sandbox", granted: started.Add(time.Second)}
				record := &model.SandboxRecord{ImageID: "sha256:image", Runtime: "runc", Runs: 1, OOM: true,
					Incomplete: alreadyIncomplete,
					Egress:     model.SandboxEgress{Allowed: map[string]uint64{}, Denied: map[string]uint64{}}}
				summary := egress.Summary{GatewayStarted: started, Incomplete: pending,
					Allowed: map[string]egress.HostCount{"api.openai.com:443": {Count: 2}},
					Denied:  map[string]egress.HostCount{"unlisted.example:443": {Count: 3}},
					Failed:  map[string]egress.HostCount{"registry.example:443": {Count: 4}}}
				got := s.egressEvidence(record, summary)
				if got.Incomplete != (pending || alreadyIncomplete) || !got.OOM || got.Runs != 1 || got.ImageID != "sha256:image" {
					t.Fatalf("collected evidence = %+v", got)
				}
				if got.Egress.Allowed["api.openai.com:443"] != 2 || got.Egress.Denied["unlisted.example:443"] != 3 ||
					got.Egress.Failed["registry.example:443"] != 4 {
					t.Fatalf("known counts lost from evidence: %+v", got.Egress)
				}
				if strings.Contains(log.String(), "unresolved requests") != pending {
					t.Fatalf("pending-request evidence log = %q", log.String())
				}
			})
		}
	}
}
