package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMeasureEntryBudgets(t *testing.T) {
	root := t.TempDir()
	for _, owner := range []string{"wide", "healthy"} {
		if err := os.Mkdir(filepath.Join(root, owner), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 20 {
		if err := os.Mkdir(filepath.Join(root, "wide", fmt.Sprint(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "healthy", "file"), []byte("count me"), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := measure(context.Background(), root, 1, 8, 100)
	if err != nil || !slices.Equal(usage.Unmeasured, []string{"wide"}) || usage.Bytes != 8 {
		t.Fatalf("one excessive owner must not hide its healthy sibling: %+v, %v", usage, err)
	}
	usage, err = measure(context.Background(), root, 1, 100, 8)
	if err != nil || !slices.Contains(usage.Unmeasured, ".") {
		t.Fatalf("exhausting total work must mark the entire measurement incomplete: %+v, %v", usage, err)
	}
}

func TestMeasureExcludesForeignRootsBeforeAccounting(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"tasks/own/file":           "primary",
		"repos/foreign-extra/file": "other",
		"repos/local/file":         "ok",
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{"repos/foreign", "repos/deep/foreign"} {
		for i := range 40 {
			path := filepath.Join(root, rel, "tasks", fmt.Sprint(i))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("foreign bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, group := range []int{0, 1, 2} {
		usage, err := measure(context.Background(), root, group, 20, 30, "repos/foreign", "repos/deep/foreign")
		if err != nil || len(usage.Unmeasured) != 0 || usage.Bytes != 14 {
			t.Fatalf("group %d counted foreign roots or exhausted their budgets: %+v, %v", group, usage, err)
		}
	}
}

// Cancel when the scan checks its caller during traversal, rather than before it starts.
type cancelDuringScan struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelDuringScan) Err() error {
	c.checks++
	if c.checks == 8 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestMeasureCancellationDuringTraversal(t *testing.T) {
	root := t.TempDir()
	for i := range 20 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &cancelDuringScan{Context: ctx, cancel: cancel}
	if _, err := measure(c, root, 0, 100, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-scan cancellation = %v", err)
	}
	if c.checks < 8 || c.checks > 9 {
		t.Fatalf("scan continued after cancellation: %d checks", c.checks)
	}
}

func TestMeasureExactEntryBudget(t *testing.T) {
	root := t.TempDir()
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := measure(context.Background(), root, 0, 4, 4)
	if err != nil || len(usage.Unmeasured) != 0 || usage.Bytes != 4 {
		t.Fatalf("an exact budget with no remaining entries is complete: %+v, %v", usage, err)
	}
}
