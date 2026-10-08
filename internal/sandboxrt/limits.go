package sandboxrt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
)

// WasmPageBytes is the size of one WebAssembly linear-memory page.
const WasmPageBytes = 65536

// Limits is the versioned resource policy of a sandbox run. Every run records
// the version and hash of the limits it ran under, so a run is always
// explained by the policy that bounded it.
type Limits struct {
	// Version names this policy. A changed limit is a new version.
	Version string `json:"version"`
	// MemoryLimitPages bounds the module's linear memory in 64 KiB pages.
	MemoryLimitPages uint32 `json:"memory_limit_pages"`
	// WallTimeMS bounds the run's wall time. When it expires the module is
	// closed wherever it is, including inside an infinite loop.
	WallTimeMS int64 `json:"wall_time_ms"`
	// MaxInputBytes bounds the canonical input document.
	MaxInputBytes int64 `json:"max_input_bytes"`
	// MaxOutputBytes bounds stdout. Reaching it stops the run.
	MaxOutputBytes int64 `json:"max_output_bytes"`
	// MaxStderrBytes bounds stderr. Reaching it stops the run.
	MaxStderrBytes int64 `json:"max_stderr_bytes"`
	// MaxRecords bounds the number of output records.
	MaxRecords int `json:"max_records"`
}

// DefaultLimits is the sandbox policy every run uses unless a caller supplies
// a stricter one.
func DefaultLimits() Limits {
	return Limits{
		Version:          "sandbox-limits/v1",
		MemoryLimitPages: 1024, // 64 MiB
		WallTimeMS:       2000,
		MaxInputBytes:    1 << 20,
		MaxOutputBytes:   256 << 10,
		MaxStderrBytes:   64 << 10,
		MaxRecords:       1000,
	}
}

// Policy bounds: a Limits outside them is refused rather than clamped.
const (
	minMemoryPages = 64    // 4 MiB: a Go module starts at about 2.2 MiB
	maxMemoryPages = 16384 // 1 GiB
	minWallTimeMS  = 10
	maxWallTimeMS  = 60_000
	minIOBytes     = 1 << 10
	maxIOBytes     = 16 << 20
	maxStderrBytes = 1 << 20
	maxRecordCount = 100_000
)

// Validate refuses a policy with a missing version or any limit outside the
// policy bounds.
func (l Limits) Validate() error {
	var problems []string
	if v := strings.TrimSpace(l.Version); v == "" || v != l.Version || len(v) > 64 {
		problems = append(problems, "version must be 1..64 characters without surrounding whitespace")
	}
	if l.MemoryLimitPages < minMemoryPages || l.MemoryLimitPages > maxMemoryPages {
		problems = append(problems, fmt.Sprintf("memory_limit_pages must be in [%d, %d]", minMemoryPages, maxMemoryPages))
	}
	if l.WallTimeMS < minWallTimeMS || l.WallTimeMS > maxWallTimeMS {
		problems = append(problems, fmt.Sprintf("wall_time_ms must be in [%d, %d]", minWallTimeMS, maxWallTimeMS))
	}
	if l.MaxInputBytes < minIOBytes || l.MaxInputBytes > maxIOBytes {
		problems = append(problems, fmt.Sprintf("max_input_bytes must be in [%d, %d]", minIOBytes, maxIOBytes))
	}
	if l.MaxOutputBytes < minIOBytes || l.MaxOutputBytes > maxIOBytes {
		problems = append(problems, fmt.Sprintf("max_output_bytes must be in [%d, %d]", minIOBytes, maxIOBytes))
	}
	if l.MaxStderrBytes < minIOBytes || l.MaxStderrBytes > maxStderrBytes {
		problems = append(problems, fmt.Sprintf("max_stderr_bytes must be in [%d, %d]", minIOBytes, maxStderrBytes))
	}
	if l.MaxRecords < 1 || l.MaxRecords > maxRecordCount {
		problems = append(problems, fmt.Sprintf("max_records must be in [1, %d]", maxRecordCount))
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "sandboxrt: invalid limits").WithField("problems", problems)
	}
	return nil
}

// MemoryLimitBytes is the memory limit in bytes.
func (l Limits) MemoryLimitBytes() uint64 { return uint64(l.MemoryLimitPages) * WasmPageBytes }

// WallTime is the wall-time limit.
func (l Limits) WallTime() time.Duration { return time.Duration(l.WallTimeMS) * time.Millisecond }

// Hash is the hex sha256 of the canonical JSON of the policy.
func (l Limits) Hash() (string, error) {
	b, err := audit.CanonicalJSON(l)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "sandboxrt: encode limits")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
