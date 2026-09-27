package store

import (
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func RedactedValue(value any) (map[string]any, error) {
	generic, err := wirejson.GenericMap(value)
	if err != nil {
		return nil, err
	}
	redact.JSON(generic)
	return generic, nil
}
