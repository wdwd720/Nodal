package payout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/terms"
)

// The withdrawal disclosure, at the boundary of this package (D-083).
//
// §48 requires the disclosure before a conversion or payout request, and the
// registry's own comment said "nothing in this package enforces that; the
// withdrawal surface asks". Nothing asked. The refusal is here, in the domain,
// because this package has more than one way in: a person pressing a button, an
// operator resolving a stuck payout, a retry, a worker.

func TestDisclosureRefusal_NamesTheDocumentAndTheStepThatFixesIt(t *testing.T) {
	t.Parallel()
	err := disclosureRefusal()
	require.Error(t, err)
	assert.Equal(t, errs.CodeTermsAcceptanceRequired, errs.CodeOf(err))

	// Not VERIFICATION_REQUIRED, which would send somebody into an identity
	// flow they may already have finished, and not FORBIDDEN, which says the
	// account may not do this at all. What is missing is a signature.
	assert.NotEqual(t, errs.CodeVerificationRequired, errs.CodeOf(err))
	assert.NotEqual(t, errs.CodeForbidden, errs.CodeOf(err))

	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, []string{"WITHDRAWAL_DISCLOSURE"}, e.Fields["documents"],
		"the surface has to know WHICH document to present")
	assert.Equal(t, "ACCEPT_TERMS", e.Fields["action"])
}

// TestRequiredDisclosure_IsTheRegistrysOwn: named through internal/terms rather
// than as a string, so a rename in the registry is a compile error here rather
// than a control that quietly stops applying.
func TestRequiredDisclosure_IsTheRegistrysOwn(t *testing.T) {
	t.Parallel()
	assert.Equal(t, terms.WithdrawalDisclosure, RequiredDisclosure)
	doc, ok := terms.Get(RequiredDisclosure)
	require.True(t, ok)
	assert.Equal(t, terms.BeforeWithdrawal, doc.Requirement,
		"this document is required at the moment value leaves, not at signup")
}

// TestCreateRequest_ValidateSaysNothingAboutTheDisclosure.
//
// Validate is the structural check and runs before anything is known about the
// person; the disclosure is a fact about them. Keeping them apart means a
// caller cannot mistake "well-formed" for "permitted".
func TestCreateRequest_ValidateSaysNothingAboutTheDisclosure(t *testing.T) {
	t.Parallel()
	acct, err := accounts.ParseAccountID(id.New[id.Any]().String())
	require.NoError(t, err)
	r := CreateRequest{
		AccountID:      acct,
		Quantity:       money.QuantityFromInt64(100),
		IdempotencyKey: "k",
		EffectiveAt:    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, r.Validate(), "an unsigned disclosure is not a malformed request")
	r.DisclosureAccepted = true
	require.NoError(t, r.Validate())
}
