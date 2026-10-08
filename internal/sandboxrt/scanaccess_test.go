package sandboxrt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestForbiddenImportAccess_ClassifiesEveryEscapeHatch: every import the
// scanner refuses as an escape hatch has a class, the longest prefix decides
// (os/exec is a process although os is the operating system), and only the
// network and process classes are network or process access.
func TestForbiddenImportAccess_ClassifiesEveryEscapeHatch(t *testing.T) {
	t.Parallel()
	for _, root := range ForbiddenImportRoots() {
		a, ok := ForbiddenImportAccess(root)
		require.True(t, ok, "the forbidden root %s has no class", root)
		assert.Contains(t, AllImportAccesses(), a, root)
	}
	for path, want := range map[string]ImportAccess{
		"net": AccessNetwork, "net/http": AccessNetwork, "net/http/httputil": AccessNetwork, "net/rpc/jsonrpc": AccessNetwork,
		"net/smtp": AccessNetwork, "net/netip": AccessNetwork, "net/url": AccessNetwork,
		"os/exec": AccessProcess, "os/signal": AccessProcess, "syscall": AccessProcess, "syscall/js": AccessProcess,
		"os": AccessOperatingSystem, "os/user": AccessOperatingSystem, "io/fs": AccessOperatingSystem, "embed": AccessOperatingSystem,
		"unsafe": AccessRuntime, "reflect": AccessRuntime, "runtime": AccessRuntime, "runtime/debug": AccessRuntime,
		"plugin": AccessRuntime,
	} {
		got, ok := ForbiddenImportAccess(path)
		require.True(t, ok, path)
		assert.Equal(t, want, got, path)
		assert.Equal(t, want == AccessNetwork || want == AccessProcess, NetworkOrProcessAccess(path), path)
		assert.True(t, forbiddenImport(path), "%s is classified, so the scanner must refuse it", path)
	}
	// Imports the scanner does not refuse as an escape hatch have no class:
	// allowed ones, ones that are merely not allowed (IMPORT_NOT_ALLOWED),
	// and names that only start like a forbidden root.
	for _, path := range append(AllowedImports(), "netx", "network/http", "golang.org/x/net/http2", "io/ioutil", "osx", "C",
		"crypto/tls", "") {
		_, ok := ForbiddenImportAccess(path)
		assert.False(t, ok, "%q", path)
		assert.False(t, NetworkOrProcessAccess(path), "%q", path)
	}
}

// TestScanReport_NetworkOrProcessImportsAreTheScannersOwnFindings: what a
// refused tree tried to reach is read from the scanner's own violations --
// the network and process imports it refused, and nothing it allowed or
// refused for another reason.
func TestScanReport_NetworkOrProcessImportsAreTheScannersOwnFindings(t *testing.T) {
	t.Parallel()
	src := Source{
		"main.go": []byte("package main\n\nimport (\n\t\"net/http\"\n\t\"os/exec\"\n\t\"reflect\"\n\t\"strings\"\n\t\"io/ioutil\"\n)\n\n" +
			"func main() { _ = http.Get; _ = exec.Command; _ = reflect.TypeOf; _ = strings.ToUpper; _ = ioutil.ReadAll }\n"),
		"util.go": []byte("package main\n\nimport \"syscall\"\n\nvar _ = syscall.Getpid\n"),
	}
	rep := Scan(src, DefaultScanLimits())
	require.False(t, rep.OK())
	assert.Contains(t, rep.Codes(), string(ScanImportForbidden))
	assert.Contains(t, rep.Codes(), string(ScanImportNotAllowed))
	assert.Equal(t, []string{"net/http", "os/exec", "syscall"}, rep.NetworkOrProcessImports())

	clean := Scan(Source{"main.go": []byte("package main\n\nimport \"reflect\"\n\nvar _ = reflect.TypeOf\n")}, DefaultScanLimits())
	require.False(t, clean.OK(), "reflect is refused")
	assert.Empty(t, clean.NetworkOrProcessImports(), "but it reaches neither the network nor a process")
}
