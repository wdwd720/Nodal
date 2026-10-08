// Package delegationtest builds delegated agent authority (internal/delegation)
// for tests that play the platform's part beside an agent runtime: the
// account owner's grants and decisions, venue registration, the delegated
// signer, submission and settlement.
//
// It exists because an agent tree's tests are held to the agent import
// boundary too (test/security/authority_boundary_test.go scans test files,
// and the depguard rule agent-authority covers them): a test of the agent
// runtime may not import internal/gates, internal/killswitch or
// internal/capital, so it cannot build the Service that decides and signs.
// NewService builds it here from a Platform -- the deployment's settings and
// the seeds only the platform holds -- with the platform readers
// delegation.NewAgentAdmission builds for the agent's side, and
// Platform.Admission is the agent's side of the same deployment, with no
// seed and no key. The test's agent holds only the admission facade while
// the test itself acts as the platform, whose grants register the delegated
// wallet keys the facade reads.
//
// What this package must never do:
//
//   - Be imported by a production file: it builds a Service that signs, and
//     scripts/lintfin (rule 2: no <pkg>/<pkg>test import outside tests)
//     refuses that mechanically.
//   - Call a Service mutator itself, or hold a seed or a key beyond the
//     Platform it is handed: it is a constructor and nothing else.
//   - Put a seed or a key into the agent's side: Platform.Admission carries
//     neither.
package delegationtest
