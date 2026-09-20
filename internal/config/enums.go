package config

import "github.com/tyk-swe/octomus-agent/internal/jsoncompat"

type Backend uint8

const (
	BackendCodex    Backend = 0
	BackendOpencode Backend = 1
)

var backendNames = []string{"codex", "opencode"}

func (v Backend) String() string               { return jsoncompat.EnumName(uint8(v), backendNames) }
func (v Backend) MarshalJSON() ([]byte, error) { return jsoncompat.MarshalEnum(uint8(v), backendNames) }
func (v *Backend) UnmarshalJSON(data []byte) error {
	n, err := jsoncompat.Enum(data, backendNames)
	if err == nil {
		*v = Backend(n)
	}
	return err
}
