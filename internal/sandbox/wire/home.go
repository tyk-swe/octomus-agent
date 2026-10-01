package wire

import "path/filepath"

// RunnerHomeDirs are where the shared runner state appears inside a runner sandbox's home, with the runner volume
// subdirectory each one mounts.
var RunnerHomeDirs = []struct{ Home, Volume string }{
	{".codex", "codex"},
	{".local/share/opencode", "opencode/data"},
	{".config/opencode", "opencode/config"},
	{".cache/opencode", "opencode/cache"},
	{".local/state/opencode", "opencode/state"},
}

const (
	RunnerHome = "home"
	VerifyHome = "verify-home"
)

// HomeDirs are the directories a sandbox of this kind mounts inside its owned root, relative to the root, in the order
// they are created.
func HomeDirs(kind string) []string {
	switch kind {
	case KindRunner:
		dirs := []string{RunnerHome}
		for _, dir := range RunnerHomeDirs {
			dirs = append(dirs, filepath.Join(RunnerHome, dir.Home))
		}
		return dirs
	case KindVerify:
		return []string{VerifyHome}
	}
	return nil
}
