package config

import "github.com/tyk-swe/octomus-agent/internal/wirejson"

type Backend uint8

const (
	BackendCodex    Backend = 0
	BackendOpencode Backend = 1
)

var backendNames = []string{"codex", "opencode"}

func (b Backend) String() string               { return wirejson.EnumName(b, backendNames) }
func (b Backend) MarshalJSON() ([]byte, error) { return wirejson.MarshalEnum(b, backendNames) }
func (b *Backend) UnmarshalJSON(data []byte) error {
	return wirejson.UnmarshalEnum(data, backendNames, b)
}
