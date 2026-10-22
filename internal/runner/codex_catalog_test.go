package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type catalogStdin struct{ bytes.Buffer }

func (*catalogStdin) Close() error                     { return nil }
func (*catalogStdin) SetWriteDeadline(time.Time) error { return nil }

// Feed RPC responses directly: catalog parsing needs neither a process nor model access.
func catalogPages(t *testing.T, pages ...any) ([]Model, error, string) {
	t.Helper()
	lines := make(chan lineResult, len(pages))
	for i, page := range pages {
		data, err := json.Marshal(map[string]any{"id": i + 1, "result": page})
		if err != nil {
			t.Fatal(err)
		}
		lines <- lineResult{line: data}
	}
	close(lines)
	stdin := &catalogStdin{}
	client := &Codex{ctx: context.Background(), stdin: stdin, lines: lines, timeout: 5}
	models, err := client.Models("")
	return models, err, stdin.String()
}

func catalogModel(name string) map[string]any {
	return map[string]any{
		"model":                     name,
		"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "medium"}},
	}
}

func catalogPage(cursor any, models ...any) map[string]any {
	if models == nil {
		models = []any{}
	}
	return map[string]any{"data": models, "nextCursor": cursor}
}

func TestCodexCatalogPagination(t *testing.T) {
	first := catalogModel("future/model")
	first["supportedReasoningEfforts"] = []any{map[string]any{"reasoningEffort": "future-effort"}}
	second := catalogModel("gpt-6.1-sol")
	second["displayName"] = "Sol"
	models, err, requests := catalogPages(t,
		catalogPage("page-two", first),
		map[string]any{"data": []any{second}}, // An absent nextCursor still terminates the catalog.
	)
	if err != nil || len(models) != 2 {
		t.Fatalf("catalog = %+v, %v", models, err)
	}
	if models[0].Model != "future/model" || models[0].DisplayName != "future/model" || models[0].Efforts[0] != "future-effort" || models[1].DisplayName != "Sol" {
		t.Fatalf("catalog values were not preserved: %+v", models)
	}
	var calls []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(requests), "\n") {
		var call map[string]any
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	if len(calls) != 2 || calls[0]["method"] != "model/list" {
		t.Fatalf("calls = %v", calls)
	}
	for i, call := range calls {
		params := call["params"].(map[string]any)
		if params["limit"] != float64(100) || params["includeHidden"] != true || (i == 0 && params["cursor"] != nil) || (i == 1 && params["cursor"] != "page-two") {
			t.Fatalf("page %d parameters = %v", i, params)
		}
	}
}

func TestCodexCatalogRejectsMalformedPages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		pages []any
		want  string
	}{
		{"missing data", []any{map[string]any{}}, "Invalid model catalog"},
		{"wrong data", []any{map[string]any{"data": map[string]any{}}}, "Invalid model catalog"},
		{"null model", []any{catalogPage(nil, nil)}, "Invalid Codex model identity"},
		{"empty model", []any{catalogPage(nil, catalogModel(""))}, "Invalid Codex model identity"},
		{"spaced model", []any{catalogPage(nil, catalogModel(" model"))}, "Invalid Codex model identity"},
		{"control in model", []any{catalogPage(nil, catalogModel("model\n"))}, "Invalid Codex model identity"},
		{"oversized model", []any{catalogPage(nil, catalogModel(strings.Repeat("m", 101)))}, "Invalid Codex model identity"},
		{"duplicate model", []any{catalogPage(nil, catalogModel("one"), catalogModel("one"))}, "Duplicate Codex model identity"},
		{"duplicate across pages", []any{catalogPage("two", catalogModel("one")), catalogPage(nil, catalogModel("one"))}, "Duplicate Codex model identity"},
		{"missing efforts", []any{catalogPage(nil, map[string]any{"model": "one"})}, "Invalid Codex reasoning catalog"},
		{"wrong effort", []any{catalogPage(nil, map[string]any{"model": "one", "supportedReasoningEfforts": []any{"medium"}})}, "Invalid reasoning effort"},
		{"empty effort", []any{catalogPage(nil, map[string]any{"model": "one", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": ""}}})}, "Invalid reasoning effort"},
		{"control in effort", []any{catalogPage(nil, map[string]any{"model": "one", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "med\nium"}}})}, "Invalid reasoning effort"},
		{"numeric cursor", []any{catalogPage(3, catalogModel("one"))}, "Invalid model pagination"},
		{"object cursor", []any{catalogPage(map[string]any{}, catalogModel("one"))}, "Invalid model pagination"},
		{"empty cursor", []any{catalogPage("", catalogModel("one"))}, "Invalid model pagination"},
		{"blank cursor", []any{catalogPage(" ", catalogModel("one"))}, "Invalid model pagination"},
		{"empty nonterminal page", []any{catalogPage("two")}, "Invalid model pagination"},
		{"repeated cursor", []any{catalogPage("two", catalogModel("one")), catalogPage("two", catalogModel("two"))}, "Invalid model pagination"},
		{"cursor cycle", []any{catalogPage("two", catalogModel("one")), catalogPage("three", catalogModel("two")), catalogPage("two", catalogModel("three"))}, "Invalid model pagination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models, err, _ := catalogPages(t, tc.pages...)
			if models != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("catalog = %v, %v; want no partial result and %q", models, err, tc.want)
			}
		})
	}
}

func TestCodexCatalogSizeBound(t *testing.T) {
	models := make([]any, 10001)
	for i := range models {
		models[i] = catalogModel(fmt.Sprintf("model-%d", i))
	}
	for _, tc := range []struct {
		name  string
		pages []any
		want  string
	}{
		{"exact bound", []any{catalogPage(nil, models[:10000]...)}, ""},
		{"oversized terminal page", []any{catalogPage(nil, models...)}, "exceeds 10000 models"},
		{"oversized later terminal page", []any{catalogPage("two", models[:1]...), catalogPage(nil, models[1:]...)}, "exceeds 10000 models"},
		{"nonterminal at bound", []any{catalogPage("two", models[:10000]...)}, "Invalid model pagination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err, _ := catalogPages(t, tc.pages...)
			if tc.want == "" {
				if err != nil || len(got) != 10000 {
					t.Fatalf("catalog length = %d, %v", len(got), err)
				}
			} else if got != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("catalog length = %d, %v; want no partial result and %q", len(got), err, tc.want)
			}
		})
	}
}
