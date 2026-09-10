package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/capacity"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/httpapi"
	"github.com/nodal/controlplane/internal/proof"
	"github.com/nodal/controlplane/internal/provider/stripecredit"
	"github.com/nodal/controlplane/internal/webhook"
)

// creditPurchaseWiring is everything the Credit purchase path needs, or
// nothing.
//
// Nothing is the normal case today and it is not a failure: a deployment with
// no payment provider configured cannot sell Credits, and saying so by leaving
// the port nil is better than half-wiring a path that would take a payment it
// could not record. Every field here is either all set or all zero.
type creditPurchaseWiring struct {
	Service *credit.PurchaseService
	// WebhookPort ingests the provider's deliveries. It is keyed into
	// Ports.Webhooks under the provider name, which is the last path segment
	// of the endpoint Stripe is configured to call.
	WebhookPort httpapi.WebhookPort
	ProviderKey string
}

// wireCreditPurchase builds the Credit purchase provider, its service and its
// webhook ingestion, or explains why it did not.
//
// It never returns an error that stops the process. A deployment that cannot
// sell Credits must still serve everything else, exactly as the funding block
// already decides. What it must NOT do is serve a purchase endpoint that
// half-works, so a partial configuration disables the whole path.
func wireCreditPurchase(
	ctx context.Context,
	cfg *config.Config,
	database *db.DB,
	resolver config.Resolver,
	clk clock.Clock,
	credits *credit.Service,
	gateChecker *gates.Checker,
	log *slog.Logger,
) creditPurchaseWiring {
	slot := cfg.Providers.CreditPurchase
	if slot.Mode == "" || slot.Name == "" {
		log.Info("credit purchase provider is not configured; selling Credits is disabled")
		return creditPurchaseWiring{}
	}

	prov, err := stripecredit.New(ctx, slot, cfg.Env, resolver, clk,
		&http.Client{Timeout: providerTimeout(slot)}, nil)
	if err != nil {
		log.Warn("credit purchase provider is not configured; selling Credits is disabled",
			"error", err.Error())
		return creditPurchaseWiring{}
	}

	// One read-only call to confirm the key belongs to the account this
	// deployment says it does. A key rotated to the wrong Stripe account is
	// otherwise silent: charges succeed, webhooks arrive, and every one of them
	// belongs to somebody else's business.
	//
	// It is a warning and a disabled path rather than a failed start, for the
	// same reason everything else here is: a deployment that cannot sell
	// Credits must still serve the rest of the product. What it must not do is
	// sell Credits through credentials nobody has checked.
	if v, ok := prov.(interface {
		VerifyAccount(context.Context) error
	}); ok {
		if verr := v.VerifyAccount(ctx); verr != nil {
			log.Warn("credit purchase credentials do not match the configured account; selling Credits is disabled",
				"error", verr.Error())
			return creditPurchaseWiring{}
		}
	}

	// The registry is where a provider that cannot safely sell Credits is
	// refused: no lookup, no idempotent create, no hosted payment UI, no
	// dispute events, or -- in production -- no contract reference. It is
	// consulted here rather than trusted to have been consulted somewhere.
	registry := credit.NewPurchaseRegistry(!cfg.Env.IsProductionLike())
	if rerr := registry.Register(prov); rerr != nil {
		log.Warn("credit purchase provider was refused; selling Credits is disabled",
			"provider", prov.Name(), "error", rerr.Error())
		return creditPurchaseWiring{}
	}

	capGuard, err := capacity.NewGuard(capacity.Budget{
		MaxAccounts:        cfg.Capacity.MaxAccounts,
		MaxPurchasesPerDay: cfg.Capacity.MaxPurchasesPerDay,
		MaxAtRiskMinor:     cfg.Capacity.MaxAtRiskMinor,
		MaxDatabaseBytes:   cfg.Capacity.MaxDatabaseBytes,
	}, clk.Now)
	if err != nil {
		log.Warn("capacity ceilings could not be built; selling Credits is disabled",
			"error", err.Error())
		return creditPurchaseWiring{}
	}
	log.Info("launch-tier capacity ceilings in force",
		"max_accounts", cfg.Capacity.MaxAccounts,
		"max_purchases_per_day", cfg.Capacity.MaxPurchasesPerDay,
		"max_at_risk_minor", cfg.Capacity.MaxAtRiskMinor,
		"max_database_bytes", cfg.Capacity.MaxDatabaseBytes)

	svc, err := credit.NewPurchaseService(credit.PurchaseServiceConfig{
		Credits:  credits,
		Provider: prov,
		// The shipped policy. Changing the rate is a new version through the
		// same path any other versioned policy takes, not an edit here.
		Pricing: credit.DefaultPricingPolicy(),
		Gates:   gateChecker,
		// The launch-tier ceilings. Built from configuration rather than
		// hardcoded, and refused outright if the configuration states none --
		// see internal/capacity, which will not construct a guard that guards
		// nothing.
		Capacity:    capGuard,
		Clock:       clk,
		Environment: string(cfg.Env),
	})
	if err != nil {
		log.Warn("credit purchase service could not be built; selling Credits is disabled",
			"error", err.Error())
		return creditPurchaseWiring{}
	}

	// Webhook ingestion. Without it a payment is taken and no Credits are ever
	// issued, so the purchase path is disabled rather than served half-built.
	port, err := creditWebhookPort(ctx, cfg, database, resolver, clk, prov, svc)
	if err != nil {
		log.Warn("credit purchase webhook ingestion could not be built; selling Credits is disabled",
			"error", err.Error())
		return creditPurchaseWiring{}
	}

	log.Info("credit purchase provider wired",
		"provider", prov.Name(), "mode", string(slot.Mode), "account_ref", slot.AccountRef,
		"shared_account", slot.Shared)
	return creditPurchaseWiring{Service: svc, WebhookPort: port, ProviderKey: prov.Name()}
}

// creditWebhookPort builds the ingestion pipeline for the provider's
// deliveries.
//
// The archive is a hard requirement and that is the pipeline's design, not an
// oversight here: step one preserves the raw signed request before anything is
// parsed, so that what a provider actually sent survives however the parse
// goes. A deployment that cannot keep that evidence must refuse the deliveries
// rather than process them unrecorded.
//
// WHERE it is kept is a different question, and one the infrastructure gets to
// answer. An object store is the usual answer. A tier without one keeps the
// objects in the application database, write-once by privilege and by trigger,
// which is the same guarantee reached differently -- see migration 00730.
func creditWebhookPort(
	ctx context.Context,
	cfg *config.Config,
	database *db.DB,
	resolver config.Resolver,
	clk clock.Clock,
	prov credit.PurchaseProvider,
	svc *credit.PurchaseService,
) (httpapi.WebhookPort, error) {
	verifier, ok := prov.(webhook.Verifier[credit.PurchaseEvent])
	if !ok {
		return nil, fmt.Errorf("credit purchase provider %q cannot verify its own webhooks", prov.Name())
	}
	evidence, err := evidenceStore(ctx, cfg, database, resolver, clk)
	if err != nil {
		return nil, err
	}
	return webhook.NewHandler(webhook.Config[credit.PurchaseEvent]{
		Verifier:   verifier,
		Dispatcher: svc,
		Archive:    evidence,
		DB:         database,
		Inbox:      event.NewInbox(clk),
		Clock:      clk,
		Tolerance:  stripecredit.SignatureTolerance,
	})
}

// webhookEvidence is what the ingestion pipeline asks of an archive: a key, a
// content type and bytes, in exchange for a reference it can record.
type webhookEvidence interface {
	Put(ctx context.Context, key, contentType string, body []byte) (string, error)
}

// evidenceStore returns the archive the configured backend names.
//
// There is no fallback between the two. A deployment that asked for an object
// store and cannot reach one does not quietly write to its database instead:
// the evidence would then be somewhere other than where the deployment's own
// runbooks say to look for it, which is a worse failure than refusing.
func evidenceStore(
	ctx context.Context,
	cfg *config.Config,
	database *db.DB,
	resolver config.Resolver,
	clk clock.Clock,
) (webhookEvidence, error) {
	switch cfg.Archive.Backend {
	case config.ArchivePostgres:
		pg, err := proof.NewPgArchive(database)
		if err != nil {
			return nil, fmt.Errorf("evidence archive: %w", err)
		}
		return pgEvidence{a: pg}, nil

	case config.ArchiveS3:
		if cfg.Archive.EvidenceBucket == "" {
			return nil, fmt.Errorf("no evidence bucket is configured; raw provider deliveries could not be preserved")
		}
		store, err := archive.NewS3(ctx, cfg.Archive, resolver, archive.S3Options{Clock: clk})
		if err != nil {
			return nil, fmt.Errorf("evidence archive: %w", err)
		}
		return evidenceArchive{store: store, bucket: cfg.Archive.EvidenceBucket}, nil

	default:
		// Unreachable through Load, which parses the backend and refuses an
		// unknown one. It is here so that adding a third backend without
		// wiring it fails at startup rather than selecting whichever branch
		// came last.
		return nil, fmt.Errorf("evidence archive: no store is wired for backend %q", string(cfg.Archive.Backend))
	}
}

// pgEvidence adapts the database-backed archive to the same interface.
//
// The content type is dropped, and that is not a loss worth carrying a column
// for: every delivery this pipeline preserves is a provider webhook body, the
// bytes are stored exactly as received, and the digest is what any later
// verification uses. A field that is always the same value is a field nobody
// reads.
type pgEvidence struct{ a *proof.PgArchive }

func (p pgEvidence) Put(ctx context.Context, key, _ string, body []byte) (string, error) {
	uri, _, err := p.a.Put(ctx, key, body, nil)
	switch {
	case err == nil:
		return uri, nil
	case errors.Is(err, proof.ErrObjectExistsIdentical):
		// A retry, not a collision. The archive key is the event id plus the
		// payload hash, so the same key with the same bytes is by construction
		// the same delivery arriving again -- which is what Stripe's at-least-
		// once contract guarantees will happen. The object is already stored,
		// unchanged, and its reference is what the caller needed.
		//
		// This is not a weakening of write-once: the archive still refused,
		// nothing was overwritten, and different bytes under the same key
		// still fail below. Treating this as an error is what made every
		// retry a 503, and a 503 asks the provider to retry again.
		return uri, nil
	default:
		return "", err
	}
}

// evidenceArchive adapts the object archive to what the webhook pipeline asks
// for: a key, a content type and bytes, in exchange for a reference.
type evidenceArchive struct {
	store  archive.ObjectArchive
	bucket string
}

func (e evidenceArchive) Put(ctx context.Context, key, contentType string, body []byte) (string, error) {
	ref, err := e.store.Put(ctx, archive.PutRequest{
		Bucket: e.bucket, Key: key, Body: body, ContentType: contentType,
	})
	if err != nil {
		return "", err
	}
	// The version-specific URI, not the bucket and key. A raw delivery is
	// evidence, and evidence that could be silently replaced by a later write
	// to the same key is not evidence.
	return ref.URI, nil
}
