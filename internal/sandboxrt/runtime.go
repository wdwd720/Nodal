package sandboxrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

// engineMemoryPages is the engine-level memory ceiling: the whole wasm32
// address space. It is deliberately not the policy limit. wazero refuses a
// growth above WithMemoryLimitPages silently -- the guest sees memory.grow
// return -1 and aborts, indistinguishable from any other abort. Every growth
// below this ceiling instead reaches the metered allocator, which enforces
// the run's own Limits.MemoryLimitPages exactly and records the refusal, so
// MEMORY_LIMIT is a fact the host observed rather than something inferred
// from what the guest printed. (A single request beyond 4 GiB is refused by
// the engine itself and ends the run as TRAP.)
const engineMemoryPages = 65536

// virtualTickNanos is how far the virtual monotonic clock advances on every
// reading.
const virtualTickNanos = 1000

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	// ModuleCacheSize bounds how many compiled artifacts stay resident
	// (default 16).
	ModuleCacheSize int
}

// Runner executes strategy artifacts. It owns one wazero runtime with WASI
// preview1 and nothing else instantiated, and a bounded cache of compiled
// modules keyed by artifact digest. Every run gets a fresh module instance:
// fresh memory, fresh stdin, fresh clock, fresh random source. A Runner is
// safe for concurrent use.
type Runner struct {
	runtime  wazero.Runtime
	capacity int

	mu      sync.Mutex
	modules map[string]*compiledEntry
	order   []string
	closed  bool
}

type compiledEntry struct {
	module      wazero.CompiledModule
	compileTime time.Duration
	users       int
	evicted     bool
}

// NewRunner builds a Runner.
func NewRunner(ctx context.Context, cfg RunnerConfig) (*Runner, error) {
	if cfg.ModuleCacheSize <= 0 {
		cfg.ModuleCacheSize = 16
	}
	rc := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(engineMemoryPages)
	rt := wazero.NewRuntimeWithConfig(ctx, rc)
	// WASI preview1 is the only host module. It has no sockets to offer
	// (no listener is ever configured) and no filesystem (no mount is ever
	// configured), so it is a clock, a random source, stdio and exit.
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, errs.Wrap(err, errs.CodeInternal, "sandboxrt: instantiate WASI preview1")
	}
	return &Runner{runtime: rt, capacity: cfg.ModuleCacheSize, modules: map[string]*compiledEntry{}}, nil
}

// Close releases the runtime and every compiled module.
func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return r.runtime.Close(ctx)
}

// Artifact is a compiled strategy: its wasm bytes and the digest they must
// hash to.
type Artifact struct {
	Digest []byte
	Wasm   []byte
}

// RunRequest is one execution.
type RunRequest struct {
	Artifact Artifact
	Input    Input
	Limits   Limits
}

// Usage is what a run consumed.
type Usage struct {
	WallTime        time.Duration `json:"wall_time"`
	CompileTime     time.Duration `json:"compile_time"`
	CacheHit        bool          `json:"cache_hit"`
	OutputBytes     int64         `json:"output_bytes"`
	StderrBytes     int64         `json:"stderr_bytes"`
	PeakMemoryBytes uint64        `json:"peak_memory_bytes"`
}

// RawRun is what the engine observed, before the output is interpreted.
type RawRun struct {
	// Termination is final for every run that did not exit 0; for a run
	// that exited 0 it is OK until Interpret reads the output.
	Termination Termination
	Detail      string
	ExitCode    uint32
	// ArtifactVerified is true when the executed bytes hashed to the
	// artifact digest.
	ArtifactVerified bool
	Stdout           []byte
	Stderr           []byte
	Usage            Usage
	ArtifactDigest   []byte
	InputDigest      []byte
	Input            []byte
}

// Engine runs an artifact. *Runner is the engine; the interface exists so the
// interpretation of output (ParseOutput, ValidateProposal, Filter) can be
// exercised over arbitrary output bytes without compiling a module per case.
type Engine interface {
	Run(ctx context.Context, req RunRequest) (RawRun, error)
}

// Run executes one artifact against one input under one limits policy. It
// returns an error only for a request that is malformed or a caller that went
// away; everything the strategy does -- loop, allocate, flood, trap, print
// garbage -- is a RawRun with a Termination.
func (r *Runner) Run(ctx context.Context, req RunRequest) (RawRun, error) {
	if err := req.Limits.Validate(); err != nil {
		return RawRun{}, err
	}
	input, err := req.Input.Canonical()
	if err != nil {
		return RawRun{}, err
	}
	if int64(len(input)) > req.Limits.MaxInputBytes {
		return RawRun{}, errs.Newf(errs.CodeValidationFailed, "sandboxrt: input is %d bytes, limit %d", len(input), req.Limits.MaxInputBytes)
	}
	if len(req.Artifact.Digest) != sha256.Size {
		return RawRun{}, errs.New(errs.CodeValidationFailed, "sandboxrt: artifact digest must be 32 bytes")
	}
	inputDigest := sha256.Sum256(input)
	raw := RawRun{ArtifactDigest: append([]byte(nil), req.Artifact.Digest...), InputDigest: inputDigest[:], Input: input}

	sum := sha256.Sum256(req.Artifact.Wasm)
	if !bytes.Equal(sum[:], req.Artifact.Digest) {
		raw.Termination, raw.Detail = TerminationInvalidModule, "artifact bytes do not hash to the artifact digest"
		return raw, nil
	}
	raw.ArtifactVerified = true

	entry, key, hit, err := r.acquire(ctx, req.Artifact)
	if err != nil {
		if ctx.Err() != nil {
			return RawRun{}, ctx.Err()
		}
		raw.Termination, raw.Detail = TerminationInvalidModule, "module does not compile: "+truncate(err.Error(), 200)
		return raw, nil
	}
	defer r.release(key)
	raw.Usage.CompileTime, raw.Usage.CacheHit = entry.compileTime, hit
	if t, detail := checkModule(entry.module, req.Limits); t != "" {
		raw.Termination, raw.Detail = t, detail
		return raw, nil
	}

	trip := &tripwire{}
	runCtx, cancelTimeout := context.WithTimeout(ctx, req.Limits.WallTime())
	defer cancelTimeout()
	runCtx, cancelRun := context.WithCancelCause(runCtx)
	defer cancelRun(nil)
	stdout := &boundedWriter{limit: req.Limits.MaxOutputBytes, trip: trip, cancel: cancelRun, stream: "stdout"}
	stderr := &boundedWriter{limit: req.Limits.MaxStderrBytes, trip: trip, cancel: cancelRun, stream: "stderr"}
	mem := &meteredMemory{limit: req.Limits.MemoryLimitBytes(), trip: trip, cancel: cancelRun}
	runCtx = experimental.WithMemoryAllocator(runCtx, experimental.MemoryAllocatorFunc(mem.allocate))

	vclock := newVirtualClock(req.Input.AsOf)
	cfg := wazero.NewModuleConfig().
		WithName("").
		WithArgs("strategy").
		WithStdin(bytes.NewReader(input)).
		WithStdout(stdout).
		WithStderr(stderr).
		WithWalltime(vclock.walltime, sys.ClockResolution(1)).
		WithNanotime(vclock.nanotime, sys.ClockResolution(1)).
		WithNanosleep(vclock.nanosleep).
		WithOsyield(func() {}).
		WithRandSource(newDeterministicRand(req.Artifact.Digest, inputDigest[:])).
		WithStartFunctions("_start")
	// Deliberately absent: WithEnv, WithFS, WithFSConfig, WithSysWalltime,
	// WithSysNanotime, WithSysNanosleep and any socket configuration.

	sw := clock.Start()
	mod, runErr := r.runtime.InstantiateModule(runCtx, entry.module, cfg)
	raw.Usage.WallTime = sw.Elapsed()
	if mod != nil {
		_ = mod.Close(context.WithoutCancel(ctx))
	}
	raw.Stdout, raw.Stderr = stdout.bytes(), stderr.bytes()
	raw.Usage.OutputBytes, raw.Usage.StderrBytes = int64(len(raw.Stdout)), int64(len(raw.Stderr))
	raw.Usage.PeakMemoryBytes = mem.peakBytes()

	if ctx.Err() != nil && trip.reason() == "" {
		// The caller went away: that is not something the strategy did.
		return RawRun{}, ctx.Err()
	}
	var exitErr *sys.ExitError
	if errors.As(runErr, &exitErr) {
		raw.ExitCode = exitErr.ExitCode()
	}
	switch reason, detail := trip.get(); {
	case reason != "":
		raw.Termination, raw.Detail = reason, detail
	case runErr == nil, exitErr != nil && raw.ExitCode == 0:
		raw.Termination = TerminationOK
	case raw.ExitCode == sys.ExitCodeDeadlineExceeded || errors.Is(runCtx.Err(), context.DeadlineExceeded):
		raw.Termination, raw.Detail = TerminationTimeout, fmt.Sprintf("wall-time limit of %dms reached", req.Limits.WallTimeMS)
	case exitErr != nil:
		raw.Termination, raw.Detail = TerminationTrap, fmt.Sprintf("exited with status %d", raw.ExitCode)
	default:
		raw.Termination, raw.Detail = TerminationTrap, truncate(runErr.Error(), 200)
	}
	return raw, nil
}

// checkModule refuses, before instantiation, a module that imports anything
// outside WASI preview1, imports a memory, has no _start, or declares more
// initial memory than the run may have.
func checkModule(m wazero.CompiledModule, limits Limits) (Termination, string) {
	for _, f := range m.ImportedFunctions() {
		module, name, _ := f.Import()
		if module != wasi_snapshot_preview1.ModuleName {
			return TerminationInvalidModule, "imports " + truncate(module, 64) + "." + truncate(name, 64) + ", which is not WASI preview1"
		}
	}
	if len(m.ImportedMemories()) > 0 {
		return TerminationInvalidModule, "imports a memory"
	}
	if _, ok := m.ExportedFunctions()["_start"]; !ok {
		return TerminationInvalidModule, "exports no _start"
	}
	for _, mem := range m.ExportedMemories() {
		if mem.Min() > limits.MemoryLimitPages {
			return TerminationMemoryLimit, fmt.Sprintf("declares %d initial pages, limit %d", mem.Min(), limits.MemoryLimitPages)
		}
	}
	return "", ""
}

// acquire returns the compiled module for an artifact, compiling it on a
// miss, and pins it against eviction until release.
func (r *Runner) acquire(ctx context.Context, a Artifact) (*compiledEntry, string, bool, error) {
	key := hex.EncodeToString(a.Digest)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, "", false, errs.New(errs.CodeInternal, "sandboxrt: runner closed")
	}
	if e, ok := r.modules[key]; ok {
		e.users++
		r.touch(key)
		r.mu.Unlock()
		return e, key, true, nil
	}
	r.mu.Unlock()

	sw := clock.Start()
	compiled, err := r.runtime.CompileModule(ctx, a.Wasm)
	if err != nil {
		return nil, "", false, err
	}
	elapsed := sw.Elapsed()

	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.modules[key]; ok {
		// Another run compiled it first; keep theirs.
		_ = compiled.Close(ctx)
		e.users++
		r.touch(key)
		return e, key, true, nil
	}
	e := &compiledEntry{module: compiled, compileTime: elapsed, users: 1}
	r.modules[key] = e
	r.order = append(r.order, key)
	r.evict(ctx)
	return e, key, false, nil
}

func (r *Runner) release(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.modules[key]
	if !ok {
		return
	}
	e.users--
	if e.evicted && e.users == 0 {
		delete(r.modules, key)
		_ = e.module.Close(context.Background())
	}
}

func (r *Runner) touch(key string) {
	for i, k := range r.order {
		if k == key {
			r.order = append(append(r.order[:i:i], r.order[i+1:]...), key)
			return
		}
	}
}

// evict drops the least recently used modules beyond capacity. A module in
// use is marked and closed when its last run releases it.
func (r *Runner) evict(ctx context.Context) {
	for len(r.order) > r.capacity {
		key := r.order[0]
		r.order = r.order[1:]
		e := r.modules[key]
		if e.users > 0 {
			e.evicted = true
			continue
		}
		delete(r.modules, key)
		_ = e.module.Close(ctx)
	}
}

// tripwire records the first resource limit a run hit. Later trips (a
// stderr flood while the runtime reports the memory refusal) do not replace
// the cause.
type tripwire struct {
	mu     sync.Mutex
	cause  Termination
	detail string
}

func (t *tripwire) set(cause Termination, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cause == "" {
		t.cause, t.detail = cause, detail
	}
}

func (t *tripwire) get() (Termination, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cause, t.detail
}

func (t *tripwire) reason() Termination {
	r, _ := t.get()
	return r
}

var (
	errOutputLimit = errors.New("sandboxrt: output limit reached")
	errMemoryLimit = errors.New("sandboxrt: memory limit reached")
)

// boundedWriter keeps at most limit bytes. The write that would exceed it
// trips the run and cancels it, so a strategy that loops writing is stopped
// by the limit rather than by the clock.
type boundedWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	limit  int64
	trip   *tripwire
	cancel context.CancelCauseFunc
	stream string
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.limit - int64(w.buf.Len())
	if int64(len(p)) <= room {
		w.buf.Write(p)
		return len(p), nil
	}
	if room > 0 {
		w.buf.Write(p[:room])
	} else {
		room = 0
	}
	w.trip.set(TerminationOutputLimit, fmt.Sprintf("%s reached its limit of %d bytes", w.stream, w.limit))
	w.cancel(errOutputLimit)
	return int(room), errOutputLimit
}

func (w *boundedWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// meteredMemory backs a module's linear memory. It is the enforcement point
// of the run's memory limit: a growth past the limit is refused (the guest
// sees memory.grow fail) and recorded as MEMORY_LIMIT. Physical memory never
// exceeds the limit: the backing slice's capacity is capped at it.
type meteredMemory struct {
	mu     sync.Mutex
	limit  uint64
	trip   *tripwire
	cancel context.CancelCauseFunc
	buf    []byte
	peak   uint64
}

func (m *meteredMemory) allocate(capacity, _ uint64) experimental.LinearMemory {
	m.mu.Lock()
	defer m.mu.Unlock()
	if capacity > m.limit {
		capacity = m.limit
	}
	m.buf = make([]byte, 0, capacity)
	return m
}

// Reallocate implements experimental.LinearMemory.
func (m *meteredMemory) Reallocate(size uint64) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if size > m.limit {
		m.trip.set(TerminationMemoryLimit, fmt.Sprintf("linear memory growth to %d bytes refused; limit %d", size, m.limit))
		// A Go guest aborts on the refusal by itself; anything else that
		// ignores it is stopped here rather than by the clock.
		m.cancel(errMemoryLimit)
		return nil
	}
	if size > m.peak {
		m.peak = size
	}
	if size <= uint64(cap(m.buf)) {
		m.buf = m.buf[:size]
		return m.buf
	}
	newCap := uint64(cap(m.buf)) * 2
	if newCap < size {
		newCap = size
	}
	if newCap > m.limit {
		newCap = m.limit
	}
	grown := make([]byte, size, newCap)
	copy(grown, m.buf)
	m.buf = grown
	return m.buf
}

// Free implements experimental.LinearMemory.
func (m *meteredMemory) Free() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf = nil
}

func (m *meteredMemory) peakBytes() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

// virtualClock is the only time a strategy sees. Wall time is the input's
// as-of time plus virtual elapsed time; the monotonic clock advances a fixed
// step on every reading and by the requested amount on every sleep. Nothing
// reads the host's clock, so the same run reads the same times.
type virtualClock struct {
	base    int64
	elapsed atomic.Int64
}

func newVirtualClock(asOf time.Time) *virtualClock {
	return &virtualClock{base: asOf.UTC().UnixNano()}
}

func (c *virtualClock) walltime() (int64, int32) {
	t := c.base + c.elapsed.Load()
	sec, nsec := t/int64(time.Second), t%int64(time.Second)
	if nsec < 0 {
		sec, nsec = sec-1, nsec+int64(time.Second)
	}
	return sec, int32(nsec) // #nosec G115 -- 0 <= nsec < 1e9
}

func (c *virtualClock) nanotime() int64 { return c.elapsed.Add(virtualTickNanos) }

func (c *virtualClock) nanosleep(ns int64) {
	if ns > 0 {
		c.elapsed.Add(ns)
	}
}

// deterministicRand is the strategy's random source: SHA-256 in counter mode
// over a seed derived from the artifact and the input. Same artifact, same
// input, same bytes -- including the Go runtime's own hash seeds, so map
// iteration order is reproducible too.
type deterministicRand struct {
	seed    [32]byte
	counter uint64
	pending []byte
}

func newDeterministicRand(artifactDigest, inputDigest []byte) *deterministicRand {
	h := sha256.New()
	h.Write([]byte("nodal.sandbox.rand/v1\n"))
	h.Write(artifactDigest)
	h.Write(inputDigest)
	r := &deterministicRand{}
	copy(r.seed[:], h.Sum(nil))
	return r
}

func (r *deterministicRand) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(r.pending) == 0 {
			var block [40]byte
			copy(block[:32], r.seed[:])
			binary.BigEndian.PutUint64(block[32:], r.counter)
			r.counter++
			sum := sha256.Sum256(block[:])
			r.pending = sum[:]
		}
		c := copy(p[n:], r.pending)
		r.pending = r.pending[c:]
		n += c
	}
	return n, nil
}
