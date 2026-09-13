//go:build windows

package agent

import "fmt"

// startShellProcess is unsupported on Windows: the persistent-shell completion
// protocol relies on a POSIX sh reading commands from a pipe, which stock
// Windows does not provide. The tool reports this to the model instead of
// silently doing nothing.
func startShellProcess(dir string, env []string) (*shellProcess, error) {
	return nil, fmt.Errorf("bash_persistent is not supported on Windows; use execute_command")
}
