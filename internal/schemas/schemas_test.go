package schemas

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestFrozenStructuredResults(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/compatibility/m1.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Structured []struct {
			Name, Kind string
			Schema     Schema
			Value      any
			Expected   *string
		}
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Structured) == 0 {
		t.Fatal("missing schema fixtures")
	}
	for _, c := range corpus.Structured {
		t.Run(c.Name, func(t *testing.T) {
			schema := c.Schema
			if c.Kind != "" {
				if c.Kind == "proposal" {
					schema = ProposalSchema()
				} else {
					schema = ReviewSchema()
				}
				raw, err := json.Marshal(schema)
				if err != nil {
					t.Fatal(err)
				}
				var normalized Schema
				if err := json.Unmarshal(raw, &normalized); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(normalized, c.Schema) {
					t.Fatal("generated schema differs from reference")
				}
			}
			err := Validate(c.Value, schema)
			if c.Expected == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != *c.Expected {
				t.Errorf("%v != %s", err, *c.Expected)
			}
		})
	}
}
