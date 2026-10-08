package sandboxrt

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A minimal WebAssembly binary encoder, enough to hand-assemble the small
// WASI modules below. They are not Go programs: they compile in
// milliseconds (also under the race detector, where compiling a Go-built
// module takes minutes), and they misbehave the way hostile non-Go code
// would -- ignoring a refused memory.grow, spinning without ever calling the
// host, importing host functions that do not exist.

const (
	valI32    = 0x7f
	opEnd     = 0x0b
	opLoop    = 0x03
	opBr      = 0x0c
	opCall    = 0x10
	opDrop    = 0x1a
	opI32     = 0x41
	opMemGrow = 0x40
	blockVoid = 0x40
)

func uleb(v uint32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

func sleb(v int32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

func name(s string) []byte { return append(uleb(uint32(len(s))), s...) }

func vec(items ...[]byte) []byte {
	out := uleb(uint32(len(items)))
	for _, it := range items {
		out = append(out, it...)
	}
	return out
}

func section(id byte, body []byte) []byte {
	return append(append([]byte{id}, uleb(uint32(len(body)))...), body...)
}

func i32(v int32) []byte { return append([]byte{opI32}, sleb(v)...) }

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

type wasmImport struct {
	module, field string
	typeIdx       uint32
}

// module assembles a module whose type section is types, whose imports
// come first in the function index space, which defines one function
// (_start, type startType, body code), exports memory (unless memPages < 0)
// and _start (unless noStart), and places data at offset 0.
func module(types [][]byte, imports []wasmImport, startType uint32, code []byte, memPages int32, data []byte, noStart bool) []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, section(1, vec(types...))...)
	if len(imports) > 0 {
		var is [][]byte
		for _, im := range imports {
			is = append(is, cat(name(im.module), name(im.field), []byte{0x00}, uleb(im.typeIdx)))
		}
		out = append(out, section(2, vec(is...))...)
	}
	out = append(out, section(3, vec(uleb(startType)))...)
	if memPages >= 0 {
		out = append(out, section(5, vec(cat([]byte{0x00}, uleb(uint32(memPages)))))...)
	}
	var exports [][]byte
	if memPages >= 0 {
		exports = append(exports, cat(name("memory"), []byte{0x02}, uleb(0)))
	}
	if !noStart {
		exports = append(exports, cat(name("_start"), []byte{0x00}, uleb(uint32(len(imports)))))
	}
	out = append(out, section(7, vec(exports...))...)
	body := cat(vec(), code, []byte{opEnd})
	out = append(out, section(10, vec(cat(uleb(uint32(len(body))), body)))...)
	if len(data) > 0 {
		out = append(out, section(11, vec(cat([]byte{0x00}, i32(0), []byte{opEnd}, uleb(uint32(len(data))), data)))...)
	}
	return out
}

var (
	typeVoid    = cat([]byte{0x60}, vec(), vec())
	typeFdWrite = cat([]byte{0x60}, vec([]byte{valI32}, []byte{valI32}, []byte{valI32}, []byte{valI32}), vec([]byte{valI32}))
)

// helloRecord is what the hello module writes: one valid log record.
const helloRecord = `{"type":"log","seq":1,"event":"tick"}` + "\n"

// writeData lays out an iovec at 0 ({ptr: 16, len}) and the bytes at 16.
func writeData(payload string) []byte {
	d := make([]byte, 16+len(payload))
	binary.LittleEndian.PutUint32(d[0:], 16)
	binary.LittleEndian.PutUint32(d[4:], uint32(len(payload)))
	copy(d[16:], payload)
	return d
}

var fdWrite = wasmImport{"wasi_snapshot_preview1", "fd_write", 1}

func callFdWrite() []byte {
	// fd_write(fd=1, iovs=0, iovs_len=1, nwritten=8); drop the errno.
	return cat(i32(1), i32(0), i32(1), i32(8), []byte{opCall}, uleb(0), []byte{opDrop})
}

func handmade(t *testing.T) map[string]Artifact {
	t.Helper()
	mods := map[string][]byte{
		// Writes one record and returns.
		"hello": module([][]byte{typeVoid, typeFdWrite}, []wasmImport{fdWrite}, 0, callFdWrite(), 1, writeData(helloRecord), false),
		// Spins forever without calling the host.
		"spin": module([][]byte{typeVoid}, nil, 0, cat([]byte{opLoop, blockVoid, opBr}, uleb(0), []byte{opEnd}), 1, nil, false),
		// Grows memory forever and ignores every refusal.
		"grow": module([][]byte{typeVoid}, nil, 0, cat([]byte{opLoop, blockVoid}, i32(16), []byte{opMemGrow, 0x00, opDrop, opBr}, uleb(0), []byte{opEnd}), 1, nil, false),
		// Writes forever and ignores every error.
		"flood": module([][]byte{typeVoid, typeFdWrite}, []wasmImport{fdWrite}, 0,
			cat([]byte{opLoop, blockVoid}, callFdWrite(), []byte{opBr}, uleb(0), []byte{opEnd}), 1, writeData(helloRecord), false),
		// Imports a host function that does not exist.
		"hostcall": module([][]byte{typeVoid}, []wasmImport{{"nodal", "sign_transaction", 0}}, 0, cat([]byte{opCall}, uleb(0)), 1, nil, false),
		// Has nothing to start.
		"nostart": module([][]byte{typeVoid}, nil, 0, nil, 1, nil, true),
		// Declares more initial memory than any run may have.
		"bigmin": module([][]byte{typeVoid}, nil, 0, nil, 2000, nil, false),
	}
	out := map[string]Artifact{}
	for n, b := range mods {
		sum := sha256.Sum256(b)
		out[n] = Artifact{Digest: sum[:], Wasm: b}
	}
	return out
}

func handmadeLimits() Limits {
	l := testLimits()
	l.MemoryLimitPages = 64
	l.WallTimeMS = 200
	l.MaxOutputBytes = 4096
	return l
}

// concurrentLimits gives 48 concurrent runs on a loaded machine room to be
// scheduled: only the spinning module ever reaches the clock.
func concurrentLimits() Limits {
	l := handmadeLimits()
	l.WallTimeMS = 2000
	return l
}

func TestRunner_HandmadeModulesAreClassifiedByTheHost(t *testing.T) {
	r, err := NewRunner(context.Background(), RunnerConfig{})
	require.NoError(t, err)
	defer func() { _ = r.Close(context.Background()) }()
	mods := handmade(t)
	cases := map[string]Termination{
		"hello": TerminationOK, "spin": TerminationTimeout, "grow": TerminationMemoryLimit, "flood": TerminationOutputLimit,
		"hostcall": TerminationInvalidModule, "nostart": TerminationInvalidModule, "bigmin": TerminationMemoryLimit,
	}
	for n, want := range cases {
		t.Run(n, func(t *testing.T) {
			start := time.Now()
			raw, err := r.Run(context.Background(), RunRequest{Artifact: mods[n], Input: testInput(), Limits: handmadeLimits()})
			require.NoError(t, err)
			assert.Equal(t, want, raw.Termination, "detail=%s", raw.Detail)
			assert.Less(t, time.Since(start), 30*time.Second)
			assert.LessOrEqual(t, raw.Usage.PeakMemoryBytes, handmadeLimits().MemoryLimitBytes())
			res, err := Interpret(raw, handmadeLimits())
			require.NoError(t, err)
			if want == TerminationOK {
				require.Len(t, res.Logs, 1)
				assert.Equal(t, "tick", res.Logs[0].Event)
			}
			if n == "flood" {
				assert.Equal(t, int64(4096), raw.Usage.OutputBytes)
			}
			if n == "hostcall" {
				assert.Contains(t, raw.Detail, "nodal.sign_transaction")
			}
		})
	}
}

// TestRunner_ConcurrentHandmadeRunsShareNothing races the host side of the
// runtime -- the compiled-module cache with its reference-counted eviction,
// the tripwire, the bounded writers and the metered memory -- with many
// concurrent runs of misbehaving modules on one Runner whose cache holds a
// single module, so every run competes for eviction.
func TestRunner_ConcurrentHandmadeRunsShareNothing(t *testing.T) {
	r, err := NewRunner(context.Background(), RunnerConfig{ModuleCacheSize: 1})
	require.NoError(t, err)
	defer func() { _ = r.Close(context.Background()) }()
	mods := handmade(t)
	order := []string{"hello", "grow", "flood", "spin", "hello", "hostcall"}
	want := map[string]Termination{"hello": TerminationOK, "grow": TerminationMemoryLimit, "flood": TerminationOutputLimit,
		"spin": TerminationTimeout, "hostcall": TerminationInvalidModule}
	var wg sync.WaitGroup
	type outcome struct {
		module string
		raw    RawRun
		err    error
	}
	results := make(chan outcome, 48)
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := order[i%len(order)]
			raw, err := r.Run(context.Background(), RunRequest{Artifact: mods[n], Input: testInput(), Limits: concurrentLimits()})
			results <- outcome{n, raw, err}
		}()
	}
	wg.Wait()
	close(results)
	var helloDigest []byte
	for o := range results {
		require.NoError(t, o.err)
		assert.Equal(t, want[o.module], o.raw.Termination, "%s: %s", o.module, o.raw.Detail)
		if o.module == "hello" {
			d := RunDigest(o.raw.ArtifactDigest, o.raw.InputDigest, o.raw.Stdout)
			if helloDigest == nil {
				helloDigest = d
			}
			assert.Equal(t, helloDigest, d, "every hello run is the same run")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	assert.LessOrEqual(t, len(r.modules), 1, "the cache stays within its bound once runs finish")
	for _, e := range r.modules {
		assert.Zero(t, e.users, "no run still holds a module")
	}
}
