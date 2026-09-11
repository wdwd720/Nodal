// Package demo seeds deterministic, clearly-labelled sandbox data for the
// internal economy (product goal §51 DEMO / STAGING DATA, §12).
//
// # What it does
//
// Creates a handful of demo native assets, opens a market for each, funds two
// demo traders with PROMOTIONAL Credits and has them trade, so a STAGING
// deployment has markets with prices, charts, positions and an activity feed
// to render. It is idempotent: every object it creates is registered in
// demo_seed_rows under a deterministic key, and a second run finds its own
// rows and creates nothing.
//
// # What it refuses to do
//
// It runs only on a sandbox tier (ADR-0023, cfg.SandboxTier()) and refuses PROD
// outright, twice: NewSeeder returns an error, and migration 00774's CHECK
// refuses a demo_seed_rows row whose environment is PROD, so a seeder that
// somehow ran there could not record having done so.
//
// It does not go around a single invariant. Every object is created through the
// same domain service a person's request goes through:
//
//	nativeasset.CreateDraft   content screening, the moderation verdict, the
//	                          registry row at RESTRICTED
//	nativeasset.SetStatus     the creator's own submission, with its transition
//	                          row and the trigger that writes the status
//	nativeasset.Activate      the economics freeze
//	nativemarket.Create       the single mint, the ledger posting, the opening
//	                          price, the instrument registration
//	nativemarket.SetStatus    the market's transition row
//	credit.Issue              a lot with an origin, a finality and a journal
//	                          transaction behind it
//	nativemarket.Execute      the curve, the risk kernel, the safety policy,
//	                          the constant-product trigger, the position and
//	                          print projections
//
// There is no raw SQL here except the INSERT into demo_seed_rows and the SELECT
// that reads it back, which is the label, not the object.
//
// It does not activate a capability gate, record a risk policy, or move a
// deployment's legal policy. A gate is a three-principal ceremony and a seeder
// that wrote one would be filling a control with fiction. On a sandbox tier the
// gates come from CP_API_SANDBOX_GATES, which is a reviewed line in the
// blueprint; where they are absent, this seeder's trades fail loudly with the
// capability's own refusal, which is the correct outcome.
//
// It does not create anything that can log in. The demo users are rows with an
// issuer of "demo", which is not the deployment's OIDC issuer, so no session can
// ever resolve to one.
//
// # Why the moderation verdict is not forced
//
// nativeasset.CreateDraft records the verdict Screen returns. The demo assets
// are written to pass screening on their own, and Seed refuses to continue if
// one does not: calling SetModeration to APPROVE the seeder's own content would
// be manufacturing a moderation decision, which is the shape of thing the goal
// forbids even where the content is harmless.
package demo
