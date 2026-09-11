// Package payouttest provides a sandbox payout provider for local development
// and tests.
//
// It is a separate package on purpose. internal/payout.Registry refuses to load
// a provider with no contract reference unless it was built with sandboxes
// allowed, and keeping the fake out of the production package means the
// production binary has no path that constructs one at all — which is the
// belt to that braces (PART LXV).
package payouttest

import (
	"context"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/payout"
)

// Sandbox is an in-memory payout provider that behaves the way a correct
// external provider would, including the ways that are inconvenient.
//
// It is deliberately faithful about idempotency and about uncertainty: the
// same key always yields the same payout, and a configured timeout returns a
// transport error AFTER recording the payout internally, which is precisely
// the case that would duplicate money if the system retried blindly.
type Sandbox struct {
	mu sync.Mutex

	name string
	caps payout.Capabilities

	// byKey is the provider's own record, keyed by the idempotency key Nodal
	// chose. A real provider has one of these; a fake that does not is a fake
	// that cannot demonstrate exactly-once.
	byKey map[string]payout.SubmitResult

	// failNext makes the next submission fail definitively.
	failNext string
	// timeoutNext makes the next submission record the payout and then return
	// a transport error, simulating a response lost in flight.
	timeoutNext bool
	// lookupDown makes Lookup fail, so a reconciliation attempt leaves the
	// request unresolved rather than resolving it wrongly.
	lookupDown bool
	// crashNext makes the next submission record the payout and then panic,
	// which is the process dying with the request committed in SUBMITTED.
	crashNext bool

	submits int
}

// NewSandbox returns a sandbox provider with no contract reference, which is
// what makes it refuse to load outside a sandbox-permitting registry.
func NewSandbox(name string) *Sandbox {
	return &Sandbox{
		name:  name,
		byKey: map[string]payout.SubmitResult{},
		caps: payout.Capabilities{
			SupportsBankPayout: true,
			SupportsFiatWallet: true,
			SupportsLookup:     true,
			SupportsWebhooks:   true,
			Currencies:         []string{"USD"},
			// No ContractReference: there is no commercial agreement behind a
			// fake, and pretending otherwise would defeat the check.
		},
	}
}

// Name identifies the provider.
func (s *Sandbox) Name() string { return s.name }

// Capabilities reports what the sandbox supports.
func (s *Sandbox) Capabilities() payout.Capabilities { return s.caps }

// WithCapabilities returns the sandbox with different capabilities, for tests
// that need a provider which cannot do something.
func (s *Sandbox) WithCapabilities(c payout.Capabilities) *Sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps = c
	return s
}

// FailNext makes the next submission fail definitively with this reason.
func (s *Sandbox) FailNext(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = reason
}

// TimeoutNext makes the next submission succeed at the provider and then lose
// the response. This is the case that duplicates money in a system that
// retries on error, so it is the one worth being able to produce on demand.
func (s *Sandbox) TimeoutNext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeoutNext = true
}

// CrashNext makes the next submission record the payout at the provider and
// then panic, which is what a process dying between Submit's phase-one commit
// and applyProviderResult looks like from inside this process.
//
// It exists because that crash window leaves a request in SUBMITTED with a
// committed idempotency key, and SUBMITTED is precisely the state F-115's
// "do not resubmit" branch got wrong. The only other way to produce it was for
// a test to write the state by hand, which migration 00807 rightly refuses:
// PAYOUT_STATUS_UNKNOWN -> SUBMITTED is an edge the state machine does not have
// and a fixture should not have been able to forge (F-226).
func (s *Sandbox) CrashNext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.crashNext = true
}

// SetLookupDown controls whether Lookup works.
func (s *Sandbox) SetLookupDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupDown = down
}

// Submits is how many payouts the provider actually recorded. A test that
// wants to prove "exactly once" asserts on this rather than on Nodal's own
// view, because Nodal's view is the thing under test.
func (s *Sandbox) Submits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byKey)
}

// Attempts is how many times Submit was called, including the ones that
// returned the stored result. It should exceed Submits whenever a retry
// happened, and the gap is the evidence that the retry was absorbed.
func (s *Sandbox) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submits
}

// Submit sends a payout, idempotently.
func (s *Sandbox) Submit(_ context.Context, req payout.SubmitRequest) (payout.SubmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submits++

	if existing, ok := s.byKey[req.IdempotencyKey]; ok {
		// The defining behaviour: the same key is the same payout, always.
		return existing, nil
	}
	if s.failNext != "" {
		reason := s.failNext
		s.failNext = ""
		res := payout.SubmitResult{
			Status: payout.ProviderFailed, RawStatus: "failed", FailureReason: reason,
		}
		s.byKey[req.IdempotencyKey] = res
		return res, nil
	}

	now := time.Now().UTC()
	res := payout.SubmitResult{
		Status:            payout.ProviderSettled,
		ProviderReference: "sbx-" + req.IdempotencyKey,
		RawStatus:         "settled",
		SettledAt:         &now,
	}
	s.byKey[req.IdempotencyKey] = res

	if s.timeoutNext {
		s.timeoutNext = false
		// Recorded, then lost. The caller learns nothing; the provider has
		// paid.
		return payout.SubmitResult{}, payout.ErrProviderUnavailable
	}
	if s.crashNext {
		s.crashNext = false
		// Recorded, and then this process stops existing. The request stays in
		// SUBMITTED, committed, with the key on disk.
		panic("payouttest: the process died after the provider took the payout")
	}
	return res, nil
}

// Corrupt rewrites what the provider will say about a key it has already
// answered for, which is how a COMPROMISED or badly broken provider behaves:
// it changes its story after the fact.
//
// The rest of this sandbox is deliberately incapable of contradicting itself,
// because a correct provider cannot. PART LXXII item 23 asks what happens when
// one does anyway, and that scenario is unreachable without an affordance that
// exists for no other purpose. It lives here, in the package the production
// registry refuses to load, and it is named after what it models rather than
// after what a test does with it.
func (s *Sandbox) Corrupt(key string, result payout.SubmitResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey[key] = result
}

// Lookup answers what happened to a key.
func (s *Sandbox) Lookup(_ context.Context, key string) (payout.SubmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookupDown {
		return payout.SubmitResult{}, payout.ErrProviderUnavailable
	}
	if res, ok := s.byKey[key]; ok {
		return res, nil
	}
	// The provider has never heard of the key, which means the submission
	// never landed. That is a definite answer and a safe one.
	return payout.SubmitResult{
		Status: payout.ProviderFailed, RawStatus: "not_found",
		FailureReason: "the provider has no record of this payout",
	}, nil
}
