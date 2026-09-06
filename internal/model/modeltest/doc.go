// Package modeltest provides a scripted model.Provider for tests.
//
// It exists so the compile pipeline can be exercised hermetically: the
// model call is the one non-deterministic step, and every stage after it
// must be provable without a network, a key or a bill.
//
// It must never:
//   - be constructed outside LOCAL, TEST or DEV — New returns
//     ErrFakeNotAllowed, so wiring it into a production binary fails at
//     startup rather than serving invented strategies;
//   - be imported by production code (enforced by scripts/lintfin and the
//     depguard rule for <pkg>test packages);
//   - reach the network, under any configuration.
package modeltest
