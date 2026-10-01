package workspace_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
	"golang.org/x/sys/unix"
)

func TestRemoveOwnedDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(filepath.Join(root, "task-1", "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "task-1", "workspace", "artifact"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RemoveOwnedDir(root, filepath.Join(root, "task-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "task-1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed child still present: %v", err)
	}
	if err := workspace.RemoveOwnedDir(root, filepath.Join(root, "task-2")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "task-3")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RemoveOwnedDir(root, link); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink cleanup = %v; want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("symlink cleanup must never touch the target")
	}
	realRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realRoot, "task-4"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkedRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(realRoot, symlinkedRoot); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RemoveOwnedDir(symlinkedRoot, filepath.Join(symlinkedRoot, "task-4")); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("cleanup under a symlinked ancestor = %v; want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(realRoot, "task-4")); err != nil {
		t.Fatal("refused cleanup must leave the real child intact")
	}
	for _, bad := range []string{".", ".."} {
		if err := workspace.RemoveOwnedDir(root, filepath.Join(root, bad)); err == nil {
			t.Fatalf("cleanup of %q must fail", bad)
		}
	}
	if err := workspace.RemoveOwnedDir("/", "/"); err == nil {
		t.Fatal("cleanup of the filesystem root must fail")
	}
	deeper := filepath.Join(root, "task-5", "workspace")
	if err := os.MkdirAll(deeper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RemoveOwnedDir(root, deeper); err == nil {
		t.Fatal("cleanup of a nested descendant must fail")
	}
	if _, err := os.Stat(deeper); err != nil {
		t.Fatal("refused cleanup must leave the path intact")
	}
}

func TestRemoveOwnedDirRemovesReadOnlyTrees(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := filepath.Join(t.TempDir(), "tasks")
	tree := filepath.Join(root, "task-1")
	for _, dir := range []string{filepath.Join(tree, "mod", "pkg"), filepath.Join(tree, "locked", "inner")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "mod", "pkg", "f.go"), []byte("package pkg\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "locked", "inner", "f.go"), []byte("package inner\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	modes := []struct {
		path string
		mode fs.FileMode
	}{
		{filepath.Join(tree, "mod", "pkg"), 0o555},
		{filepath.Join(tree, "mod"), 0o555},
		{filepath.Join(tree, "locked", "inner"), 0o555},
		{filepath.Join(tree, "locked"), 0o000},
		{tree, 0o555},
	}
	for _, m := range modes {
		if err := os.Chmod(m.path, m.mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for i := len(modes) - 1; i >= 0; i-- {
			_ = os.Chmod(modes[i].path, 0o755)
		}
	})
	if err := workspace.RemoveOwnedDir(root, tree); err != nil {
		t.Fatalf("read-only owned tree cleanup = %v; want removal", err)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only owned tree still present: %v", err)
	}
}

func TestRemoveOwnedDirRejectsNoncanonicalPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for _, dir := range []string{filepath.Join(outside, "child"), filepath.Join(outside, "victim"), filepath.Join(root, "victim")} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "child"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/link/../victim", "/./victim", "//victim", "/victim/"} {
		t.Run(suffix, func(t *testing.T) {
			if err := workspace.RemoveOwnedDir(root, root+suffix); err == nil {
				t.Fatal("noncanonical cleanup path must be refused")
			}
			for _, dir := range []string{root, outside} {
				if _, err := os.Stat(filepath.Join(dir, "victim")); err != nil {
					t.Fatalf("refused cleanup must preserve victim in %s: %v", dir, err)
				}
			}
		})
	}
}

func TestMeasure(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "1234")
	write("sub/b.txt", "123456")
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(target), filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	usage, err := workspace.Measure(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Bytes != 10 || len(usage.Unmeasured) != 0 {
		t.Fatalf("usage = %+v; want 10 bytes (symlink targets excluded), all measured", usage)
	}
	if usage, err := workspace.Measure(filepath.Join(root, "missing"), 0); err != nil || usage.Bytes != 0 {
		t.Fatalf("missing tree = %+v, %v; want zero", usage, err)
	}
}

// nest builds a chain of depth directories, each named name, under base one level at a time, as a sandbox can with
// relative mkdir and cd, so no path it uses is longer than one name. It returns the deepest directory's descriptor.
func nest(t *testing.T, base, name string, depth int) int {
	t.Helper()
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range depth {
		if err := unix.Mkdirat(fd, name, 0o755); err != nil {
			t.Fatal(err)
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		fd = next
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func TestMeasureReportsTooDeepSubtreesWithoutFailing(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tasks", "t1", "workspace")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "shallow.txt"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Spelled out as one path, this chain is far beyond PATH_MAX.
	nest(t, tree, "a", 2200)

	for group, want := range map[int]string{
		0: ".",
		2: filepath.Join("tasks", "t1"),
		3: filepath.Join("tasks", "t1", "workspace"),
	} {
		usage, err := workspace.Measure(root, group)
		if err != nil {
			t.Fatalf("Measure of a too-deep tree = %v; want the reachable bytes", err)
		}
		if usage.Bytes != 5 || !slices.Equal(usage.Unmeasured, []string{want}) {
			t.Fatalf("group %d: usage = %d bytes, unmeasured %q; want 5 bytes and the deep chain reported once as %q", group, usage.Bytes, usage.Unmeasured, want)
		}
	}
	if err := workspace.RemoveOwnedDir(filepath.Join(root, "tasks"), filepath.Join(root, "tasks", "t1")); err != nil {
		t.Fatalf("removing a too-deep tree = %v", err)
	}
}

// A sandbox can leave any number of unmeasurable directories under long names at the depth limit or behind a denial;
// the report holds one entry per owner and never one path per directory.
func TestMeasureReportsEachOwnerOnceWhateverTheFanOut(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	long := strings.Repeat("n", 250)
	// tasks/t1/workspace sits 3 levels down, so 2108 more reach the deepest level the walk opens.
	deepest := nest(t, filepath.Join(root, "tasks", "t1", "workspace"), long, 2108)
	file, err := unix.Openat(deepest, "counted.txt", unix.O_WRONLY|unix.O_CREAT|unix.O_CLOEXEC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(file, []byte("123")); err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(file)
	for i := range 2000 {
		if err := unix.Mkdirat(deepest, fmt.Sprintf("%s-%04d", long[:200], i), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	denied := filepath.Join(root, "tasks", "t2", "workspace")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		path := filepath.Join(denied, fmt.Sprintf("locked-%03d", i))
		if err := os.Mkdir(path, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
	}

	usage, err := workspace.Measure(root, 2)
	if err != nil {
		t.Fatalf("Measure with a wide fan-out of unmeasurable directories = %v", err)
	}
	if want := []string{filepath.Join("tasks", "t1"), filepath.Join("tasks", "t2")}; usage.Bytes != 3 || !slices.Equal(usage.Unmeasured, want) {
		t.Fatalf("usage = %d bytes, unmeasured %d entries %.200q; want 3 bytes and exactly %q", usage.Bytes, len(usage.Unmeasured), usage.Unmeasured, want)
	}
}

// lowerDescriptorLimit is used only in subprocesses, so changing the process-wide limit cannot affect other tests.
func lowerDescriptorLimit(t *testing.T) {
	t.Helper()
	// Open a regular file first, so the runtime's own descriptors exist before counting.
	if f, err := os.Open(os.Args[0]); err == nil {
		_ = f.Close()
	}
	open, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("needs /proc/self/fd")
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	// The listing includes its own, now-closed descriptor, leaving three available for the walk.
	limit.Cur = min(limit.Cur, uint64(len(open)+2))
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
}

func TestMeasureKeepsDescriptorUseBounded(t *testing.T) {
	const env = "OCTOMUS_MEASURE_BOUNDED_DESCRIPTORS"
	if root := os.Getenv(env); root != "" {
		lowerDescriptorLimit(t)
		// Repeat to catch leaked descriptors as well as descriptors retained along a deep traversal. The normal
		// tree has siblings at its deepest level, so returning to them must reopen the closed ancestors safely.
		for range 3 {
			usage, err := workspace.Measure(root, 2)
			if err != nil {
				t.Fatalf("Measure of deep trees under a descriptor limit = %v", err)
			}
			if usage.Bytes != 20 || !slices.Equal(usage.Unmeasured, []string{filepath.Join("tasks", "too-deep")}) {
				t.Fatalf("usage = %+v; want 20 bytes and only tasks/too-deep unmeasured", usage)
			}
		}
		return
	}
	root := t.TempDir()
	deepest := nest(t, filepath.Join(root, "tasks", "normal", "workspace"), strings.Repeat("n", 80), 128)
	for _, name := range []string{"left", "right"} {
		if err := unix.Mkdirat(deepest, name, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkdirat(deepest, name+"/sub", 0o755); err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Openat(deepest, name+"/sub/counted.txt", unix.O_WRONLY|unix.O_CREAT|unix.O_CLOEXEC, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, err = unix.Write(fd, []byte("1234567"))
		_ = unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
	}
	// A wide fanout of leaf directories must retain their parent rather than reopening the entire deep path for
	// every sibling. The non-leaf siblings above still force the bounded-descriptor ancestor-reopen path.
	for i := range 1000 {
		if err := unix.Mkdirat(deepest, fmt.Sprintf("leaf-%04d", i), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "normal", "sibling.txt"), []byte("123456"), 0o644); err != nil {
		t.Fatal(err)
	}
	nest(t, filepath.Join(root, "tasks", "too-deep", "workspace"), "a", 2200)
	child := exec.Command(os.Args[0], "-test.run=^TestMeasureKeepsDescriptorUseBounded$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), env+"="+root)
	if out, err := child.CombinedOutput(); err != nil || !strings.Contains(string(out), "--- PASS") {
		t.Fatalf("measuring deep trees under a descriptor limit: %v\n%s", err, out)
	}
}

// Genuine process-wide descriptor exhaustion still fails the measurement rather than hiding a subtree. Unlike a
// sandbox's deep directory chain, other users of the process's descriptors are not under the walk's control.
func TestMeasureFailsOnErrorsASandboxCannotCause(t *testing.T) {
	const env = "OCTOMUS_MEASURE_UNDER_FD_LIMIT"
	if root := os.Getenv(env); root != "" {
		lowerDescriptorLimit(t)
		var held []int
		defer func() {
			for _, fd := range held {
				_ = unix.Close(fd)
			}
		}()
		for {
			fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.EMFILE) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, fd)
		}
		if len(held) == 0 {
			t.Fatal("no descriptor available for measured root")
		}
		// Leave precisely one descriptor for the measured root; even its immediate child cannot be opened.
		_ = unix.Close(held[len(held)-1])
		held = held[:len(held)-1]
		usage, err := workspace.Measure(root, 2)
		if !errors.Is(err, unix.EMFILE) {
			t.Fatalf("Measure out of descriptors = %+v, %v; want EMFILE", usage, err)
		}
		return
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestMeasureFailsOnErrorsASandboxCannotCause$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), env+"="+root)
	if out, err := child.CombinedOutput(); err != nil || !strings.Contains(string(out), "--- PASS") {
		t.Fatalf("measuring under a descriptor limit: %v\n%s", err, out)
	}
}

func TestMeasureReportsUnreadableSubtrees(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "tasks", "t1", "workspace")
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(workspaceDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("readable.txt", "1234567")
	write("locked/inner/f.txt", "hidden")
	write("nosearch/f.txt", "hidden too")
	locked := []struct {
		path string
		mode fs.FileMode
	}{
		{filepath.Join(workspaceDir, "locked"), 0o000},
		{filepath.Join(workspaceDir, "nosearch"), 0o644},
	}
	for _, dir := range locked {
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, dir := range locked {
			_ = os.Chmod(dir.path, 0o755)
		}
	})
	usage, err := workspace.Measure(root, 4)
	if err != nil {
		t.Fatalf("Measure with unreadable subtrees = %v; want the readable bytes", err)
	}
	if usage.Bytes != 7 {
		t.Fatalf("size = %d; want 7 (the readable file only)", usage.Bytes)
	}
	want := []string{filepath.Join("tasks", "t1", "workspace", "locked"), filepath.Join("tasks", "t1", "workspace", "nosearch")}
	if !slices.Equal(usage.Unmeasured, want) {
		t.Fatalf("unmeasured = %q; want %q, whose bytes are unknown", usage.Unmeasured, want)
	}
	if usage, err := workspace.Measure(root, 2); err != nil || !slices.Equal(usage.Unmeasured, []string{filepath.Join("tasks", "t1")}) {
		t.Fatalf("unmeasured grouped by owner = %q, %v; want the owning task once", usage.Unmeasured, err)
	}
	for _, dir := range locked {
		info, err := os.Lstat(dir.path)
		if err != nil || info.Mode().Perm() != dir.mode {
			t.Fatalf("measurement changed %s: %v, %v; want mode %v", dir.path, info, err, dir.mode)
		}
	}
	for _, dir := range locked {
		if _, err := workspace.Measure(dir.path, 0); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("unreadable measured root %s = %v; want a permission error", dir.path, err)
		}
	}
}

func TestInitialized(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	session := "session-1"
	task := model.Task{Workspace: checkout, ComparisonBase: "abc", ExecutionSession: &session}
	if !workspace.Initialized(task) {
		t.Fatal("a task with session, base and checkout must be initialized")
	}
	noSession := task
	noSession.ExecutionSession = nil
	if workspace.Initialized(noSession) {
		t.Fatal("a task without a recorded session is not initialized")
	}
	noBase := task
	noBase.ComparisonBase = ""
	if workspace.Initialized(noBase) {
		t.Fatal("a task without a comparison base is not initialized")
	}
	noCheckout := task
	noCheckout.Workspace = filepath.Join(root, "missing")
	if workspace.Initialized(noCheckout) {
		t.Fatal("a task without a real checkout is not initialized")
	}
}
