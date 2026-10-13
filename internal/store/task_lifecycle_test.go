package store_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestTaskCommitRejectsStaleCheckpoint(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	expected := task()
	expected.Status = model.StatusExecuting
	must(t, store.Put(s, store.Tasks.Key(expected.ID), expected))
	first := expected.Clone()
	first.Error = new("First observation")
	committed, err := s.CommitTask(expected, first, model.TaskCheckpoint)
	must(t, err)
	stale := expected.Clone()
	stale.Status = model.StatusReviewing
	if _, err := s.CommitTask(expected, stale, model.TaskCheckpoint); !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("stale checkpoint: %v", err)
	}
	if got := savedPublicationTask(t, s, expected.ID); !wirejson.Equal(got, committed) {
		t.Fatal("stale write replaced the canonical observation")
	}
}

func TestCancellationPreventsCheckpointAndRetainsRepairEvidence(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	expected := task()
	expected.Status, expected.RepairRounds = model.StatusRepairing, new(uint64(0))
	expected.Sessions = []model.Session{model.NewSession("repair-1", "repair", expected.Route)}
	must(t, store.Put(s, store.Tasks.Key(expected.ID), expected))
	must(t, s.MarkCancel(expected.ID))
	changed, err := s.CancelTask(expected.ID)
	must(t, err)
	if !changed {
		t.Fatal("cancellation was not committed")
	}
	pending := expected.Clone()
	pending.Sessions[0].MarkCompleted("Repair finished while cancellation was requested")
	pending.RepairRounds = new(uint64(1))
	pending.RepairProgress = &model.RepairProgress{AwaitingReview: true}
	if _, err := s.CommitTask(expected, pending, model.TaskCheckpoint); !errors.Is(err, model.ErrTaskCancelled) {
		t.Fatalf("checkpoint after cancellation: %v", err)
	}
	if got := savedPublicationTask(t, s, expected.ID); got.Status != model.StatusCancelled || *got.RepairRounds != 0 {
		t.Fatal("late checkpoint replaced cancellation")
	}
	committed, err := s.CommitTask(expected, pending, model.TaskSettlement)
	must(t, err)
	if committed.Status != model.StatusCancelled || *committed.RepairRounds != 1 || committed.Sessions[0].Status != model.SessionCompleted {
		t.Fatal("settlement lost cancellation or completed repair evidence")
	}
	if _, err := s.CommitTask(expected, pending, model.TaskSettlement); !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("duplicate repair settlement: %v", err)
	}
	if got := savedPublicationTask(t, s, expected.ID); *got.RepairRounds != 1 {
		t.Fatal("repair completion was charged twice")
	}
}

func TestStaleRetryPreservesCancellationAndPolicy(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	expected := task()
	expected.Status = model.StatusBlocked
	must(t, store.Put(s, store.Tasks.Key(expected.ID), expected))
	must(t, s.MarkCancel(expected.ID))
	newer := expected.Clone()
	newer.Error = new("New diagnostic")
	must(t, store.Put(s, store.Tasks.Key(expected.ID), newer))
	retry := expected.Clone()
	retry.Status, retry.Attempts = model.StatusQueued, expected.Attempts+1
	retry.AttemptPolicy = new(model.PolicyOf(expected.Config))
	if err := s.RetryTask(expected, retry); !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("stale retry: %v", err)
	}
	marked, err := s.CancelMarked(expected.ID)
	must(t, err)
	if !marked || !wirejson.Equal(savedPublicationTask(t, s, expected.ID), newer) {
		t.Fatal("refused retry changed the task or its cancellation marker")
	}
	var count int
	must(t, raw(t, path).QueryRow(`SELECT count(*) FROM events WHERE kind='attempt_policy' AND entity_id=?`, expected.ID).Scan(&count))
	if count != 0 {
		t.Fatal("refused retry recorded an adopted policy")
	}
}

func TestTaskWritesUseGuardedBoundary(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(source))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				}
				if name == "SaveTask" {
					t.Errorf("unguarded task writer in %s: %s", path, fn.Name.Name)
				}
				if (name == "Put" || name == "txPut") && len(call.Args) > 1 && taskKeyExpression(call.Args[1]) {
					allowed := path == filepath.Join(root, "store", "task_lifecycle.go") && (fn.Name.Name == "txCommitTask" || fn.Name.Name == "insertTask")
					if !allowed {
						t.Errorf("task write bypasses policy in %s: %s", path, fn.Name.Name)
					}
				}
				return true
			})
		}
		return nil
	})
	must(t, err)
}

func taskKeyExpression(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	method, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || method.Sel.Name != "Key" {
		return false
	}
	switch owner := method.X.(type) {
	case *ast.Ident:
		return owner.Name == "Tasks"
	case *ast.SelectorExpr:
		return owner.Sel.Name == "Tasks"
	}
	return false
}
