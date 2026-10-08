package sandboxrt

import "strings"

// What a forbidden import would reach.
//
// The scanner refuses every import under ForbiddenImportRoots with
// IMPORT_FORBIDDEN, naming the import path in the violation's detail. The
// runtime gives a module no socket and no process to reach (it has no such
// host import), so a strategy that wanted the network or another process can
// only be seen trying here, at scan time: ForbiddenImportAccess says which of
// the refused imports were such an attempt (advanced_sandbox_network_violations
// counts the refusals with one).

// ImportAccess is what a forbidden import would give a strategy.
type ImportAccess string

// Import access classes.
const (
	// AccessNetwork: net and every package under it -- sockets and the
	// protocols over them. The scanner refuses the whole tree as the
	// network, and so is it counted, the address and message parsers under
	// it included.
	AccessNetwork ImportAccess = "NETWORK"
	// AccessProcess: os/exec and os/signal, which exist to run and signal
	// other processes, and syscall with everything under it, the raw system
	// calls beneath every other check (sockets, processes and files alike).
	AccessProcess ImportAccess = "PROCESS"
	// AccessOperatingSystem: os itself, os/user, io/fs and embed -- files,
	// the environment, the user database and files read at build time. os
	// can also start a process (os.StartProcess), but it is what a strategy
	// imports for a file, the environment or an exit status, so only the
	// packages that exist to run processes are counted as process access.
	AccessOperatingSystem ImportAccess = "OPERATING_SYSTEM"
	// AccessRuntime: unsafe, reflect, runtime with everything under it, and
	// plugin -- escaping the language's memory and type safety, or loading
	// code.
	AccessRuntime ImportAccess = "RUNTIME"
)

// AllImportAccesses returns every class.
func AllImportAccesses() []ImportAccess {
	return []ImportAccess{AccessNetwork, AccessProcess, AccessOperatingSystem, AccessRuntime}
}

// importAccessRules map an import path prefix, at a path boundary, to its
// class. The longest matching prefix decides, so os/exec is a process even
// though os is the operating system.
var importAccessRules = []struct {
	prefix string
	access ImportAccess
}{
	{"net", AccessNetwork},
	{"os/exec", AccessProcess},
	{"os/signal", AccessProcess},
	{"syscall", AccessProcess},
	{"os", AccessOperatingSystem},
	{"io/fs", AccessOperatingSystem},
	{"embed", AccessOperatingSystem},
	{"unsafe", AccessRuntime},
	{"reflect", AccessRuntime},
	{"runtime", AccessRuntime},
	{"plugin", AccessRuntime},
}

// ForbiddenImportAccess classifies an import path the scanner refuses with
// IMPORT_FORBIDDEN. ok is false for a path the scanner does not refuse as an
// escape hatch (an allowed import, or one merely not allowed).
func ForbiddenImportAccess(path string) (access ImportAccess, ok bool) {
	if !forbiddenImport(path) {
		return "", false
	}
	best := -1
	for _, r := range importAccessRules {
		if (path == r.prefix || strings.HasPrefix(path, r.prefix+"/")) && len(r.prefix) > best {
			best, access = len(r.prefix), r.access
		}
	}
	return access, best >= 0
}

// NetworkOrProcessAccess reports whether a forbidden import would give a
// strategy the network or another process: an attempt the sandbox would
// otherwise have to stop at run time.
func NetworkOrProcessAccess(path string) bool {
	a, ok := ForbiddenImportAccess(path)
	return ok && (a == AccessNetwork || a == AccessProcess)
}

// NetworkOrProcessImports returns the import paths of a scan report's
// IMPORT_FORBIDDEN violations that would give the strategy the network or
// another process, in the report's order.
func (r ScanReport) NetworkOrProcessImports() []string {
	var out []string
	for _, v := range r.Violations {
		if v.Code == ScanImportForbidden && NetworkOrProcessAccess(v.Detail) {
			out = append(out, v.Detail)
		}
	}
	return out
}
