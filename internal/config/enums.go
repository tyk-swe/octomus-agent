package config

import "github.com/tyk-swe/octomus-agent/internal/wirejson"

type Backend uint8

const (
	BackendCodex    Backend = 0
	BackendOpencode Backend = 1
)

var backendNames = []string{"codex", "opencode"}

func (v Backend) String() string               { return wirejson.EnumName(v, backendNames) }
func (v Backend) MarshalJSON() ([]byte, error) { return wirejson.MarshalEnum(v, backendNames) }
func (v *Backend) UnmarshalJSON(data []byte) error {
	return wirejson.UnmarshalEnum(data, backendNames, v)
}
