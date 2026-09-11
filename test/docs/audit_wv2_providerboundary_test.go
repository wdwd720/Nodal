package docsref

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/verification"
)

// Reproduction for the second round of the withdrawal-verification audit
// (goal §54). It changes no product code.

// ---------------------------------------------------------------------------
// F-wv2-10 — PROVIDER_BOUNDARY.md §3b, the section F-234's own fix wrote to
// say "what the shipped interfaces ACTUALLY ARE", names a method
// verification.Provider does not have and omits one it does.
//
// It says:
//
//	`verification.Provider` (`internal/verification/provider.go`): `Name`,
//	`Capabilities`, `Start`, `Poll` and `ParseWebhook`.
//
// The interface is `Name`, `Capabilities`, `Start`, `Get`, `Resume` and
// `ParseWebhook`. `Poll` is the SERVICE's method name (verification.Service.Poll
// calls provider.Get); `Resume` is a shipped method that §3a lists as a design
// target and the "gap between 3a and 3b" paragraph does not list as missing --
// so a reader who trusts §3b concludes that resuming a hosted session is still
// to be built, which is the exact failure F-234 was: "A design document that
// reads as an inventory is worse than no document, because the difference is
// only discovered by somebody who assumed it."
//
// Nothing holds §3b to the code: TestDocs_EveryPathTheyNameExists checks route
// paths and TestDocs_CountsMatchTheCode checks numbered claims, and neither
// reads a method list.
// ---------------------------------------------------------------------------

func TestAuditWV2_ProviderBoundaryNamesTheShippedInterfaceMethods(t *testing.T) {
	t.Parallel()
	const path = "docs/product/PROVIDER_BOUNDARY.md"
	body, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(path)))
	require(t, err == nil, "reading %s: %v", path, err)
	doc := string(body)

	for _, iface := range []struct {
		name string
		typ  reflect.Type
	}{
		{"verification.Provider", reflect.TypeOf((*verification.Provider)(nil)).Elem()},
		{"payout.Provider", reflect.TypeOf((*payout.Provider)(nil)).Elem()},
	} {
		var missing []string
		for i := 0; i < iface.typ.NumMethod(); i++ {
			m := iface.typ.Method(i).Name
			if !strings.Contains(doc, "`"+m+"`") &&
				!strings.Contains(doc, "`"+m+"(") &&
				!strings.Contains(doc, "`"+m+" ") {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s §3b claims to list what %s actually is and never names %v",
				path, iface.name, missing)
		}
	}

	// The other half: a method §3b names that no shipped interface has. `Poll`
	// belongs to verification.Service, and §3b attributes it to the provider
	// contract.
	if strings.Contains(doc, "`Start`, `Poll` and `ParseWebhook`") {
		t.Errorf("%s §3b names `Poll` as a verification.Provider method; the interface method is `Get` "+
			"(verification.Service.Poll is the caller)", path)
	}
}
