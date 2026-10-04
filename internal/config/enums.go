package config

import "github.com/tyk-swe/octomus-agent/internal/wirejson"

type Backend uint8

const (
	BackendCodex    Backend = 0
	BackendOpencode Backend = 1
)

var backendNames = []string{"codex", "opencode"}

func (b Backend) String() string               { return wirejson.EnumName(b, backendNames) }
func (b Backend) MarshalText() ([]byte, error) { return wirejson.EnumText(b, backendNames) }
func (b *Backend) UnmarshalText(text []byte) error {
	value, err := wirejson.ParseEnum(text, backendNames)
	if err == nil {
		*b = Backend(value)
	}
	return err
}
