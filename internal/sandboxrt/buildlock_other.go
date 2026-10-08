//go:build !unix

package sandboxrt

import "os"

// lockBuildDir has no lock to take where flock(2) is not available: a build
// directory is still removed when its build ends, but one left by a process
// that died is never recognized as abandoned.
func lockBuildDir(string) (*os.File, error) { return nil, nil }

// buildDirAbandoned never finds an abandoned directory without locks.
func buildDirAbandoned(string) bool { return false }
