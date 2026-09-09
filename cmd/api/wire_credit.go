package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/httpapi"
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

	svc, err := credit.NewPurchaseService(credit.PurchaseServiceConfig{
		Credits:  credits,
		Provider: prov,
		// The shipped policy. Changing the rate is a new version through the
		// same path any other versioned policy takes, not an edit here.
		Pricing:     credit.DefaultPricingPolicy(),
		Gates:       gateChecker,
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
// goes. A deployment with no object store cannot keep that evidence, and the
// honest response is to refuse the deliveries rather than process them
// unrecorded.
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
	if cfg.Archive.EvidenceBucket == "" {
		return nil, fmt.Errorf("no evidence bucket is configured; raw provider deliveries could not be preserved")
	}
	store, err := archive.NewS3(ctx, cfg.Archive, resolver, archive.S3Options{Clock: clk})
	if err != nil {
		return nil, fmt.Errorf("evidence archive: %w", err)
	}
	return webhook.NewHandler(webhook.Config[credit.PurchaseEvent]{
		Verifier:   verifier,
		Dispatcher: svc,
		Archive:    evidenceArchive{store: store, bucket: cfg.Archive.EvidenceBucket},
		DB:         database,
		Inbox:      event.NewInbox(clk),
		Clock:      clk,
		Tolerance:  stripecredit.SignatureTolerance,
	})
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
