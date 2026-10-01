package wire

// Request.Runner names a runner, as its program is called inside the sandbox image.
const (
	RunnerCodex    = "codex"
	RunnerOpenCode = "opencode"
)

// Runners are every runner a sandbox can serve.
var Runners = []string{RunnerCodex, RunnerOpenCode}

// VersionFailed joins a runner's name and why its --version failed on a line of the version probe's stderr.
const VersionFailed = " --version failed: "

// RunnerArgs are the fixed arguments each runner is served with, or nil for an unknown runner.
func RunnerArgs(name string) []string {
	switch name {
	case RunnerCodex:
		return []string{"app-server", "--listen", "stdio://"}
	case RunnerOpenCode:
		return []string{"serve", "--hostname", "127.0.0.1", "--port", "0"}
	}
	return nil
}

// VerifyProgram is the program and arguments that run one verification command.
func VerifyProgram(command string) (string, []string) {
	return "bash", []string{"-o", "pipefail", "-c", command}
}
