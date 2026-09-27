package runner

import (
	"fmt"
	"sort"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

// Catalog allowlists /provider output; the document can contain credentials
// and options that must never leave the adapter.
func Catalog(value any) ([]Model, error) {
	doc, _ := asObject(value)
	all, ok := asArray(doc["all"])
	if !ok {
		return nil, fmt.Errorf("Invalid OpenCode provider catalog")
	}
	connectedRaw, ok := asArray(doc["connected"])
	if !ok {
		return nil, fmt.Errorf("Missing OpenCode provider availability")
	}
	connected := map[string]bool{}
	for _, c := range connectedRaw {
		if s, ok := c.(string); ok {
			connected[s] = true
		}
	}
	out := []Model{}
	for _, p := range all {
		provider, _ := asObject(p)
		id, ok := strAt(provider, "id")
		if !ok {
			return nil, fmt.Errorf("Missing OpenCode provider identity")
		}
		available := connected[id]
		models, ok := asObject(provider["models"])
		if !ok {
			return nil, fmt.Errorf("Invalid OpenCode models")
		}
		modelIDs := make([]string, 0, len(models))
		for modelID := range models {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, modelID := range modelIDs {
			if len(out) >= 10000 {
				return nil, fmt.Errorf("OpenCode catalog exceeds 10000 models")
			}
			m, _ := asObject(models[modelID])
			capabilities, _ := asObject(m["capabilities"])
			toolcall := capabilities["toolcall"] == true
			input, _ := asObject(capabilities["input"])
			output, _ := asObject(capabilities["output"])
			text := input["text"] == true && output["text"] == true
			variants := []string{}
			if v, ok := asObject(m["variants"]); ok {
				for name := range v {
					variants = append(variants, name)
				}
				sort.Strings(variants)
			}
			display := modelID
			if s, ok := strAt(m, "name"); ok {
				display = s
			}
			var providerName *string
			if s, ok := strAt(provider, "name"); ok {
				providerName = stringPtr(s)
			}
			var reason *string
			switch {
			case !available:
				reason = stringPtr("Provider is not configured; configure OpenCode as the service user")
			case !toolcall || !text:
				reason = stringPtr("Model must support text and tool calling")
			}
			out = append(out, Model{
				Backend:           config.BackendOpencode,
				Provider:          stringPtr(id),
				ProviderName:      providerName,
				Model:             modelID,
				DisplayName:       display,
				Efforts:           []string{},
				Variants:          variants,
				Available:         available && toolcall && text,
				UnavailableReason: reason,
			})
		}
	}
	return out, nil
}
