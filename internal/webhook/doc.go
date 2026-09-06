// Package webhook is the generic ingestion pipeline for inbound provider
// events (goal PART 30). One Handler serves one provider adapter and runs the
// ten mandatory steps for every request, in this order:
//
//  1. read and preserve the raw request body (size-capped, archived);
//  2. verify the provider signature through the adapter;
//  3. enforce the signature timestamp tolerance (replay window);
//  4. hash the payload (SHA-256);
//  5. extract the provider event id;
//  6. insert the provider_events evidence row under
//     UNIQUE(provider, provider_event_id) and the inbox row through
//     event.Inbox.ProcessHashed, both in one transaction;
//  7. acknowledge a duplicate with 200 and no side effects;
//  8. dispatch to the domain handler inside that same transaction;
//  9. leave the canonical domain event to the domain handler, which writes
//     it to the outbox in the same transaction (the funding dispatcher emits
//     funding.deposit.transitioned; no generic "provider event received"
//     topic exists in internal/event, so the pipeline itself emits nothing);
//  10. record processing status (PROCESSED, IGNORED, FAILED) and the error.
//
// A failed signature or a stale timestamp answers 400 and persists exactly
// one thing: a security_events row (webhook_signature_failed or
// webhook_timestamp_stale). Nothing else is written. A domain error rolls
// the transaction back, records FAILED on the evidence and inbox rows in a
// separate transaction, and answers 500 so the provider redelivers; the
// inbox re-runs FAILED messages, so a fixed handler processes the retry.
//
// This package must never:
//   - perform a money operation outside the inbox transaction, or from a
//     request whose evidence row has not been persisted;
//   - treat an HTTP 200 it returns as a statement about settlement: 200
//     means "received and deduplicated", nothing more;
//   - trust the provider event id, type, or timestamp before the signature
//     has verified;
//   - accept a signature scheme other than the one the adapter implements,
//     or compare signatures with anything but a constant-time function;
//   - process the same provider event twice, or a message id whose payload
//     hash differs from the first delivery;
//   - log or archive a body before it has been size-capped.
package webhook
