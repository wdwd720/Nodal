//go:build !unix

package sandboxrt

import "os/exec"

// configureProcessGroup keeps the default cancellation (the go command is
// killed; WaitDelay bounds how long its children may hold the pipes) where
// process groups are not available.
func configureProcessGroup(*exec.Cmd) {}
