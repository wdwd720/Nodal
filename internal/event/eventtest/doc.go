// Package eventtest provides test doubles for internal/event: an in-memory
// Bus with consumer groups, per-key ordering and fault injection, and a
// Recorder that captures published messages.
//
// # Responsibilities
//
//   - MemoryBus delivers every message to every subscribed consumer group at
//     least once, in publish order, with messages of one key always handled
//     by the same handler of a group (the in-memory analog of a partition).
//     Late subscribers replay the retained log from the beginning.
//   - Fault injection: DuplicateEvery(n) redelivers every nth message,
//     ReorderWindow(k) shuffles the order of different keys within windows
//     of k messages (per-key order is preserved, as Redpanda would),
//     FailPublishFor(topic, n) rejects the next n publishes on a topic.
//   - Recorder stores what was published and can be told to fail.
//
// # What this package must never do
//
//   - Exist in a production binary: NewMemoryBus refuses every environment
//     except LOCAL, TEST and DEV, and internal/... production code never
//     imports a *test package (scripts/lintfin enforces it).
//   - Persist anything: delivered state lives in memory and is lost with the
//     process, which is exactly why it is not a Bus for PROD.
package eventtest
