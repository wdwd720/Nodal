//go:build darwin

package sandboxrt

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// groupRSS is the resident memory, in bytes, of every process in a process
// group. Darwin has no procfs, so it asks ps(1) -- by absolute path, with the
// group as its only argument -- which reports each member's resident set in
// KiB.
func groupRSS(ctx context.Context, pgid int) (uint64, bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "rss=", "-g", strconv.Itoa(pgid)).Output() // #nosec G204 -- a fixed program; the argument is a number
	if err != nil {
		return 0, false
	}
	var total uint64
	for _, line := range strings.Fields(string(out)) {
		kib, err := strconv.ParseUint(line, 10, 64)
		if err != nil {
			return 0, false
		}
		total += kib * 1024
	}
	return total, true
}

// peakRSS is the largest resident set the finished command or any process it
// waited for held: Darwin reports ru_maxrss in bytes, and the rusage of a
// reaped child includes its own reaped children.
func peakRSS(ps *os.ProcessState) uint64 {
	if ps == nil {
		return 0
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru.Maxrss <= 0 {
		return 0
	}
	return uint64(ru.Maxrss) // #nosec G115 -- positive by the check above
}
