package config

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/jsoncompat"
)

type wireFixture struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Input    string         `json:"input"`
	Expected map[string]any `json:"expected"`
}

func TestFrozenWireContracts(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/compatibility/m1.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []wireFixture `json:"cases"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, c := range corpus.Cases {
		var target any
		switch c.Type {
		case "Config":
			target = new(Config)
		case "Route":
			target = new(Route)
		case "Backend":
			target = new(Backend)
		default:
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			err := json.Unmarshal([]byte(c.Input), target)
			if c.Expected["rejected"] == true {
				if err == nil {
					t.Fatalf("accepted invalid input: %s", c.Input)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected reference input: %v; %s", err, c.Input)
			}
			output, err := jsoncompat.Marshal(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(output) != c.Expected["json"] {
				t.Errorf("wire bytes differ\n Go: %s\nRust: %s", output, c.Expected["json"])
			}

			switch v := target.(type) {
			case *Config:
				checkError(t, v.Validate(false), c.Expected["validation_error"])
				fingerprint, err := v.Fingerprint()
				if err != nil {
					t.Fatal(err)
				}
				if fingerprint != c.Expected["fingerprint"] {
					t.Errorf("fingerprint: %s != %v", fingerprint, c.Expected["fingerprint"])
				}
			case *Route:
				checkError(t, v.Validate(false), c.Expected["validation_error"])
				checkError(t, v.Validate(true), c.Expected["ready_error"])
				if v.String() != c.Expected["display"] {
					t.Errorf("route display: %s", v)
				}
			}
		})
	}
}
func checkError(t *testing.T, err error, want any) {
	t.Helper()
	var got any
	if err != nil {
		got = err.Error()
	}
	if got != want {
		t.Errorf("error: %v; want %v", got, want)
	}
}
