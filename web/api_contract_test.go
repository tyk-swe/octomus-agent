package dashboard_test

import (
	"encoding"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/export"
	"github.com/tyk-swe/octomus-agent/internal/httpapi"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

type pageItem struct{}

// apiContract derives the dashboard declarations from actual endpoint envelopes
// and recursively referenced JSON types. It honors flattening, omitempty,
// pointer nullability, collection element types and text enums. Dynamic evidence
// remains unknown; this does not infer success from historical arbitrary JSON.
func apiContract() string {
	roots := map[string]any{
		"Snapshot": engine.StateView{}, "Task": engine.TaskDetail{}, "Cycle": model.Cycle{},
		"TaskRow": store.TaskSummary{}, "CycleSummary": store.CycleSummary{},
		"PRObservation": store.PRSummary{}, "PR": store.PullRequestSummary{},
		"ProposalRow": store.ProposalSummary{}, "ProposalDetail": store.ProposalDetail{},
		"Config": config.Config{}, "Route": config.Route{}, "Model": runner.Model{},
		"SettingsView": engine.SettingsView{}, "BaselineView": engine.BaselineView{},
		"BaselineSummary": engine.BaselineSummary{}, "BaselineCheck": model.BaselineCheck{},
		"Event": model.Event{}, "RunEvidenceV1": export.RunEvidenceV1{},
		"DoctorResponse": httpapi.DoctorResponse{}, "ControlResult": engine.ControlResult{}, "ControlAction": model.ControlAction(0), "ApiFailure": httpapi.ErrorResponse{},
		"RequestsView": engine.RequestsView{}, "OperatorRequest": model.OperatorRequest{}, "RequestAction": model.RequestAction(0),
		"Insights":       store.Insights{},
		"HealthResponse": httpapi.HealthResponse{}, "RepositoriesView": httpapi.RepositoriesView{}, "RepositorySummary": httpapi.RepositorySummary{}, "DoctorFailure": httpapi.DoctorFailure{}, "Acknowledgement": httpapi.Acknowledgement{}, "TransformedField": redact.DisplayTransform{},
	}
	names := map[reflect.Type]string{
		reflect.TypeFor[model.Status]():     "TaskStatus",
		reflect.TypeFor[export.Revisions](): "EvidenceRevisions",
		reflect.TypeFor[model.Grounding]():  "Grounding",
	}
	for name, root := range roots {
		names[reflect.TypeOf(root)] = name
	}
	definitions := map[string]string{}
	var typeName func(reflect.Type) string
	var shape func(reflect.Type) string
	var fields func(reflect.Type) string
	literal := func(values ...string) string {
		for i := range values {
			values[i] = "'" + values[i] + "'"
		}
		return strings.Join(values, " | ")
	}
	overrides := map[reflect.Type]string{
		reflect.TypeFor[engine.ActionErrorCode](): literal(string(engine.ActionConflict), string(engine.ConfigRevisionConflict), string(engine.ActiveWorkConflict), string(engine.UnresolvedWorkConflict), string(engine.RecoveryPendingConflict)),
		reflect.TypeFor[export.VerdictState]():    literal(string(export.VerdictRecorded), string(export.VerdictMissing), string(export.VerdictDuplicate), string(export.VerdictMalformed)),
		reflect.TypeFor[export.ChecksState]():     literal(string(export.ChecksNotConfigured), string(export.ChecksRecorded)),
		reflect.TypeFor[export.CommandState]():    literal(string(export.CommandPassed), string(export.CommandPassedElsewhere), string(export.CommandFailed), string(export.CommandNoResult)),
	}
	fieldOverride := func(record reflect.Type, field reflect.StructField) string {
		switch {
		case record == reflect.TypeFor[engine.SettingsView]() && field.Name == "Config":
			// Display transformation preserves config's container/leaf shape, while
			// transformed_fields names values which cannot be sent back unchanged.
			return typeName(reflect.TypeFor[config.Config]())
		case record == reflect.TypeFor[engine.SandboxPosture]() && field.Name == "Egress",
			record == reflect.TypeFor[wire.BrokerInfo]() && (field.Name == "ImageDigests" || field.Name == "Runners" || field.Name == "RunnerErrors"):
			// Unlike durable records, these posture responses preserve nil collections.
			return typeName(field.Type) + " | null"
		case record == reflect.TypeFor[redact.DisplayTransform]() && field.Name == "Paths":
			return "(string | number)[][]"
		case record == reflect.TypeFor[redact.DisplayTransform]() && field.Name == "Kinds":
			return "('redacted' | 'shortened')[]"
		}
		return ""
	}
	fields = func(record reflect.Type) string {
		var result strings.Builder
		for i := range record.NumField() {
			field := record.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			if !field.IsExported() || tag[0] == "-" {
				continue
			}
			if field.Anonymous && tag[0] == "" {
				result.WriteString(fields(field.Type))
				continue
			}
			name := tag[0]
			if name == "" {
				name = field.Name
			}
			optional := ""
			if slices.Contains(tag[1:], "omitempty") || slices.Contains(tag[1:], "omitzero") {
				optional = "?"
			}
			typ := fieldOverride(record, field)
			if typ == "" {
				typ = typeName(field.Type)
			}
			fmt.Fprintf(&result, "  %s%s: %s;\n", name, optional, typ)
		}
		return result.String()
	}
	shape = func(record reflect.Type) string {
		if override, ok := overrides[record]; ok {
			return override
		}
		if record.Kind() == reflect.Uint8 {
			if _, ok := reflect.Zero(record).Interface().(fmt.Stringer); ok {
				if _, ok := reflect.Zero(record).Interface().(encoding.TextMarshaler); !ok {
					panic(fmt.Sprintf("API enum %s has display text but serializes as a number; expose its wire string explicitly", record))
				}
				var values []string
				for n := range 256 {
					value := reflect.New(record).Elem()
					value.SetUint(uint64(n))
					data, err := json.Marshal(value.Interface())
					if err != nil {
						break
					}
					var word string
					if err := json.Unmarshal(data, &word); err != nil {
						panic(fmt.Sprintf("API enum %s has non-string wire encoding", record))
					}
					values = append(values, word)
				}
				return literal(values...)
			}
		}
		switch record.Kind() {
		case reflect.Pointer:
			return typeName(record.Elem()) + " | null"
		case reflect.Struct:
			return "{\n" + fields(record) + "}"
		case reflect.Slice, reflect.Array:
			element := typeName(record.Elem())
			if strings.Contains(element, " | ") {
				element = "(" + element + ")"
			}
			return element + "[]"
		case reflect.Map:
			return "Record<string, " + typeName(record.Elem()) + ">"
		case reflect.String:
			return "string"
		case reflect.Bool:
			return "boolean"
		case reflect.Interface:
			return "unknown"
		default:
			return "number"
		}
	}
	typeName = func(record reflect.Type) string {
		if record == reflect.TypeFor[pageItem]() {
			return "T"
		}
		if record.Name() == "" || record.PkgPath() == "" {
			return shape(record)
		}
		name, ok := names[record]
		if !ok {
			name = record.Name()
			name = strings.ToUpper(name[:1]) + name[1:]
			names[record] = name
		}
		if _, exists := definitions[name]; !exists {
			definitions[name] = "" // Break cycles before traversing nested fields.
			definitions[name] = shape(record)
		}
		return name
	}
	for _, root := range roots {
		typeName(reflect.TypeOf(root))
	}
	var out strings.Builder
	out.WriteString("// Generated from Go API and evidence types by api_contract_test.go.\n// Regenerate: OCTOMUS_UPDATE_API_CONTRACT=1 go test ./web -run TestAPIContractGenerated\n\n")
	for _, name := range slices.Sorted(maps.Values(names)) {
		// Several reflected aliases can point at one name; definitions owns output.
		definition, ok := definitions[name]
		if !ok {
			continue
		}
		delete(definitions, name)
		fmt.Fprintf(&out, "export type %s = %s;\n", name, definition)
	}
	fmt.Fprintf(&out, "\nexport type Page<T> = %s;\n", shape(reflect.TypeFor[store.Page[pageItem]]()))
	out.WriteString("export type ModelCatalog = { binary: string; models: Model[]; loaded: boolean; error?: string };\n")
	var active []string
	for _, status := range model.ActiveStatuses() {
		active = append(active, "'"+status.String()+"'")
	}
	fmt.Fprintf(&out, "export const ACTIVE_STATUSES: string[] = [%s];\n", strings.Join(active, ", "))
	var failures []string
	for _, kind := range model.ErrorEventKinds() {
		failures = append(failures, "'"+kind.String()+"'")
	}
	fmt.Fprintf(&out, "export const ERROR_EVENT_KINDS: EventKind[] = [%s];\n", strings.Join(failures, ", "))
	return out.String()
}

func TestAPIContractGenerated(t *testing.T) {
	const path = "src/lib/types.ts"
	expected := apiContract()
	if os.Getenv("OCTOMUS_UPDATE_API_CONTRACT") == "1" {
		if err := os.WriteFile(path, []byte(expected), 0644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != expected {
		t.Fatal("Go endpoint contract changed; regenerate types.ts with OCTOMUS_UPDATE_API_CONTRACT=1 go test ./web -run TestAPIContractGenerated and migrate dashboard callers")
	}
}
