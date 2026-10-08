//go:build linux

package sandboxrt

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// groupRSS is the resident memory, in bytes, of every process in a process
// group: /proc/<pid>/stat field 5 is the group and field 24 the resident set
// in pages. A process that exits between the listing and the read is simply
// not counted.
func groupRSS(_ context.Context, pgid int) (uint64, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	page := uint64(os.Getpagesize()) // #nosec G115 -- a page size is positive
	var total uint64
	for _, e := range entries {
		name := e.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		b, err := os.ReadFile("/proc/" + name + "/stat") // #nosec G304 -- a procfs path built from a pid
		if err != nil {
			continue
		}
		// The command name may hold spaces and parentheses; the fields that
		// follow it start after the last ')'. fields[0] is field 3 (state).
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(b[i+1:]))
		if len(fields) < 22 {
			continue
		}
		if pg, err := strconv.Atoi(fields[2]); err != nil || pg != pgid {
			continue
		}
		pages, err := strconv.ParseUint(fields[21], 10, 64)
		if err != nil {
			continue
		}
		total += pages * page
	}
	return total, true
}

// peakRSS is the largest resident set the finished command or any process it
// waited for held: Linux reports ru_maxrss in KiB, and the rusage of a reaped
// child includes its own reaped children.
func peakRSS(ps *os.ProcessState) uint64 {
	if ps == nil {
		return 0
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru.Maxrss <= 0 {
		return 0
	}
	return uint64(ru.Maxrss) * 1024 // #nosec G115 -- positive by the check above
}
