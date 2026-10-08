//go:build !linux && !darwin

package sandboxrt

import (
	"context"
	"os"
)

// groupRSS cannot measure a process group on this platform: a running build
// is bounded by its time and output limits only.
func groupRSS(context.Context, int) (uint64, bool) { return 0, false }

// peakRSS cannot read a finished command's peak memory on this platform.
func peakRSS(*os.ProcessState) uint64 { return 0 }
