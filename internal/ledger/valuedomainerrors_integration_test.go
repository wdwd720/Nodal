//go:build integration

package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// A value-domain refusal reaches its caller classified (F-58).
//
// Migration 00710 raises five SQLSTATEs across thirteen sites, and until this
// test none of them appeared in any Go file. `internal/valuedomain` had no
// `SQLState` call and no error mapping, and `ledger.MapError` did not consult
// it -- so a posting refused for moving Credits into real capital came back as
// `INTERNAL`: a 500 to the customer, a page to whoever is on call, and a message
// that does not say which rule fired.
//
// The existing coverage could not have caught it. Every value-domain test in
// this package asserts on the *text* of the database's message
// ("VALUE_DOMAIN_FORBIDDEN", "may never move together"), which is present with
// or without a mapping. Nothing asserted on what the caller receives, which is
// the thing an operator and a customer actually see.

func TestIntegration_ValueDomainRefusalsAreClassified(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	credit := creditAsset(t)

	f.seed(CodeCreditBalance, credit, 10_000)
	f.seed(CodeWallet, f.sol, 10_000)

	creditAcct, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeCreditBalance, credit))
	require.NoError(t, err)
	creditSink, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodeMarketReserve, credit))
	require.NoError(t, err)
	solAcct, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeWallet, f.sol))
	require.NoError(t, err)
	solSource, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodePlatformAdjustment, f.sol))
	require.NoError(t, err)

	// A balanced transaction that the ledger's own triggers have no objection
	// to, and that only the value-domain rule refuses.
	raw := rawPosting(t, rawTx{
		kind: "NATIVE_TRADE",
		entries: []rawEntryRow{
			{acct: creditAcct.ID, asset: credit, side: "CREDIT", qty: 1000},
			{acct: creditSink.ID, asset: credit, side: "DEBIT", qty: 1000},
			{acct: solAcct.ID, asset: f.sol, side: "DEBIT", qty: 1000},
			{acct: solSource.ID, asset: f.sol, side: "CREDIT", qty: 1000},
		},
	})
	require.Error(t, raw, "the database must refuse Credits and self-custodial crypto in one transaction")

	// What the caller gets.
	mapped := MapError(raw)
	require.Error(t, mapped)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(mapped),
		"a domain-model refusal is a malformed request, not an internal fault: %v", mapped)
	assert.Contains(t, mapped.Error(), "may never move together")

	// And through the package that owns the rule, which is where a caller with
	// no ledger dependency reaches it.
	assert.True(t, valuedomain.IsValueDomainViolation(raw))
	assert.Equal(t, "value domain rule "+valuedomain.SQLStateForbiddenPair, valuedomain.Describe(raw))

	// The negative control, and the reason this is not simply "call it
	// validation failed": an error that is not a value-domain refusal must fall
	// through, or the mapping would swallow everything.
	assert.Nil(t, valuedomain.MapError(errs.New(errs.CodeInternal, "something else entirely")))
	assert.Empty(t, valuedomain.Describe(errs.New(errs.CodeInternal, "something else entirely")))
	assert.Nil(t, valuedomain.MapError(nil))
}

// TestIntegration_EveryValueDomainCodeIsMapped drives the other reachable
// codes, so the mapping is not one case that happens to work.
func TestIntegration_EveryValueDomainCodeIsMapped(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	credit := creditAsset(t)

	f.seed(CodeCreditBalance, credit, 10_000)
	f.seed(CodeWallet, f.sol, 10_000)

	creditAcct, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeCreditBalance, credit))
	require.NoError(t, err)
	creditSink, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodeMarketReserve, credit))
	require.NoError(t, err)

	// A single-domain transaction that declares a conversion it did not make:
	// VD003, the rule that holds the boundary between simulated and real
	// capital, and the one most worth having a message for.
	raw := rawPosting(t, rawTx{
		kind: "NATIVE_TRADE", convFrom: "INTERNAL_CREDIT", convTo: "SELF_CUSTODIAL_CRYPTO",
		entries: []rawEntryRow{
			{acct: creditAcct.ID, asset: credit, side: "CREDIT", qty: 1000},
			{acct: creditSink.ID, asset: credit, side: "DEBIT", qty: 1000},
		},
	})
	require.Error(t, raw)
	require.Equal(t, valuedomain.SQLStateUndeclaredConversion, db.SQLState(raw), "got %v", raw)

	mapped := valuedomain.MapError(raw)
	require.Error(t, mapped)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(mapped))
	assert.Contains(t, mapped.Error(), "declared conversion")
}
