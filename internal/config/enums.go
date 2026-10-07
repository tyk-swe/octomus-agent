package config

import "github.com/tyk-swe/octomus-agent/internal/wirejson"

type Backend uint8

const (
	BackendCodex    Backend = 0
	BackendOpencode Backend = 1
)

var backendNames = []string{"codex", "opencode"}

func (b Backend) String() string                   { return wirejson.EnumName(b, backendNames) }
func (b Backend) MarshalText() ([]byte, error)     { return wirejson.EnumText(b, backendNames) }
func (b *Backend) UnmarshalText(text []byte) error { return wirejson.ParseEnum(b, text, backendNames) }

type DeliveryMode uint8

const (
	DeliveryModeStandard    DeliveryMode = 0
	DeliveryModeMaintenance DeliveryMode = 1
)

var deliveryModeNames = []string{"standard", "maintenance"}

func (d DeliveryMode) String() string               { return wirejson.EnumName(d, deliveryModeNames) }
func (d DeliveryMode) MarshalText() ([]byte, error) { return wirejson.EnumText(d, deliveryModeNames) }
func (d *DeliveryMode) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(d, text, deliveryModeNames)
}
