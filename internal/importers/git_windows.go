//go:build windows

package importers

import "os/exec"

// configureGitCmd keeps the default process kill on Windows, where process
// groups work differently; gitWaitDelay still bounds the output-pipe drain when
// a surviving child holds a pipe open.
func configureGitCmd(cmd *exec.Cmd) {}
