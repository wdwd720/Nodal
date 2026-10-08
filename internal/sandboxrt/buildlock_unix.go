//go:build unix

package sandboxrt

import (
	"os"
	"path/filepath"
	"syscall"
)

// buildLockName is the lock file of a build directory.
const buildLockName = ".lock"

// lockBuildDir marks a build directory as in use by this process for as long
// as the returned file stays open: an exclusive flock(2) on dir/.lock. The
// file is created under another name and renamed into place only once it is
// locked, so a sweeper never finds a lock file it could take before its
// owner holds it. The lock goes with the process, however it ends.
func lockBuildDir(dir string) (*os.File, error) {
	tmp := filepath.Join(dir, buildLockName+".new")
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a path in a directory the builder created
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor
		_ = f.Close()
		return nil, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, buildLockName)); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// buildDirAbandoned reports whether a build directory's lock file exists and
// nobody holds its lock: the process that made it is gone.
func buildDirAbandoned(dir string) bool {
	f, err := os.Open(filepath.Join(dir, buildLockName)) // #nosec G304 -- a lock file in the builder's work area
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil // #nosec G115 -- a file descriptor
}
