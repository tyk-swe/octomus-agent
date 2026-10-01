package wire

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
