package verification

import (
	"context"
	"strings"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// SandboxOutcome is what a sandbox operator chooses for a rehearsal session.
//
// There is no default. A sandbox session nobody has answered stays
// PENDING_USER_ACTION forever, because "approved unless told otherwise" is the
// exact shape of a fabricated approval and the goal forbids one (ADR-0023, and
// the common brief's forbidden list).
//
// The five values are the five outcomes the withdrawal journey has to be able
// to rehearse. Three of them are refusals, and two of those are refusals for
// reasons §21 says must be separately expressible — an age failure and a
// sanctions failure are not the same event and must not be exercised as one.
type SandboxOutcome string

// Sandbox outcomes.
const (
	// SandboxVerified passes every sub-check.
	SandboxVerified SandboxOutcome = "VERIFIED"
	// SandboxNeedsInformation is the provider asking for more.
	SandboxNeedsInformation SandboxOutcome = "NEEDS_INFORMATION"
	// SandboxRejected is a failed document check.
	SandboxRejected SandboxOutcome = "REJECTED"
	// SandboxUnderage passes the document and fails the age check.
	SandboxUnderage SandboxOutcome = "UNDERAGE"
	// SandboxSanctioned passes document, age and jurisdiction and fails the
	// sanctions screen.
	SandboxSanctioned SandboxOutcome = "SANCTIONED"
)

var allSandboxOutcomes = []SandboxOutcome{
	SandboxVerified, SandboxNeedsInformation, SandboxRejected, SandboxUnderage, SandboxSanctioned,
}

// AllSandboxOutcomes returns every declared sandbox outcome (a copy).
func AllSandboxOutcomes() []SandboxOutcome {
	return append([]SandboxOutcome(nil), allSandboxOutcomes...)
}

// Valid reports whether o is declared.
func (o SandboxOutcome) Valid() bool {
	for _, x := range allSandboxOutcomes {
		if x == o {
			return true
		}
	}
	return false
}

func (o SandboxOutcome) String() string { return string(o) }

// ParseSandboxOutcome parses the canonical uppercase form.
func ParseSandboxOutcome(in string) (SandboxOutcome, error) {
	o := SandboxOutcome(strings.ToUpper(strings.TrimSpace(in)))
	if !o.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed,
			"unknown sandbox verification outcome %q; it must be one of %v", in, AllSandboxOutcomes())
	}
	return o, nil
}

// SandboxController is the extra surface a sandbox verification provider
// exposes: a way for a person driving a rehearsal to say what the provider
// should decide.
//
// It is deliberately NOT part of Provider. A real provider must not be able to
// implement it, and a code path that reaches it has to have obtained a
// SandboxController specifically, which means the type system carries the
// distinction rather than a boolean somewhere.
type SandboxController interface {
	Provider
	// SetOutcome records what this rehearsal session should decide. It returns
	// an error for a session it has never seen: a sandbox provider does not
	// invent sessions any more than a real one does.
	SetOutcome(providerRef string, outcome SandboxOutcome) error
}

// SetSandboxOutcome chooses the outcome of a rehearsal verification and applies
// it immediately.
//
// Every refusal below happens before anything is written:
//
//  1. the deployment must be a sandbox tier (`cfg.SandboxTier()`);
//  2. the configured provider must be a SandboxController, which only
//     `internal/provider/verifysandbox` is;
//  3. the person must have an open session, which they only have if they
//     started one.
//
// The result it produces is marked sandbox at every layer: the provider marks
// the Result, the service writes `sandbox = true` on every evidence row, the
// CHECK in migration 00762 refuses that combination in PROD, and every API
// response that shows the outcome repeats the label.
func (s *Service) SetSandboxOutcome(ctx context.Context, database *db.DB, accountID accounts.AccountID, outcome SandboxOutcome) (Session, error) {
	if !s.deps.SandboxTier {
		return Session{}, errs.New(errs.CodeForbidden,
			"choosing a verification outcome is a sandbox-tier affordance; this deployment is not a sandbox tier")
	}
	if !outcome.Valid() {
		return Session{}, errs.Newf(errs.CodeValidationFailed,
			"unknown sandbox verification outcome %q", outcome)
	}
	provider, err := s.Provider()
	if err != nil {
		return Session{}, err
	}
	controller, ok := provider.(SandboxController)
	if !ok {
		return Session{}, errs.Newf(errs.CodeForbidden,
			"the configured verification provider %q is not a sandbox provider and its outcome is not ours to choose",
			provider.Name())
	}
	owner, err := s.owner(ctx, database, accountID)
	if err != nil {
		return Session{}, err
	}
	session, found, err := s.deps.Repo.OpenSession(ctx, database, owner)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, errs.New(errs.CodeNotFound,
			"there is no verification in progress; start one before choosing its outcome")
	}
	if session.ProviderRef == "" {
		return Session{}, errs.New(errs.CodeConflict,
			"that verification session was never handed to the provider")
	}
	if err := controller.SetOutcome(session.ProviderRef, outcome); err != nil {
		return Session{}, err
	}
	result, err := controller.Get(ctx, session.ProviderRef)
	if err != nil {
		return Session{}, err
	}
	return s.Ingest(ctx, database, session, result,
		"a sandbox outcome was chosen explicitly: "+string(outcome)+" (this is a rehearsal, not an approval)",
		string(outcome))
}
