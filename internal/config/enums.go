package config

import "github.com/tyk-swe/octomus-agent/internal/wirejson"

type Backend uint8

const (
	BackendCodex    Backend = 0
	BackendOpencode Backend = 1
)

var backendNames = []string{"codex", "opencode"}

func (v Backend) String() string               { return wirejson.EnumName(uint8(v), backendNames) }
func (v Backend) MarshalJSON() ([]byte, error) { return wirejson.MarshalEnum(uint8(v), backendNames) }
func (v *Backend) UnmarshalJSON(data []byte) error {
	n, err := wirejson.Enum(data, backendNames)
	if err == nil {
		*v = Backend(n)
	}
	return err
}
