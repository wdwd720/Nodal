package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/alert"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// The production caller F-118 said did not exist.
//
// `reconciliation.Metrics.OnAlert` has been a seam with nothing attached to it
// since it was written. Every alert this system raises -- a ledger-integrity
// violation, an unknown submission, an unauthorised signing candidate, an
// observer disagreement -- incremented a counter the process discarded on exit
// and, since the last batch, wrote a log line. Nothing left the building.
//
// This attaches a dispatcher to that seam, so an alert reaches a destination
// the deployment named.
//
// ## Why the wiring lives here and not in internal/reconciliation
//
// Because the destination is a deployment fact and reconciliation is a domain
// package. `internal/reconciliation` must not know that a webhook exists, for
// the same reason it does not know that Render exists. The composition root is
// where a fact about the world is allowed in.

// alertDeliveryTimeoutHeadroom keeps one delivery from outliving the shutdown
// budget it drains inside.
const alertDeliveryTimeoutHeadroom = 2 * time.Second

// newAlertDispatcher builds the dispatcher, or returns nil when no destination
// is configured -- and an error when this deployment is one that may not run
// without one.
//
// A nil dispatcher is a working configuration in LOCAL, TEST and DEV: a
// developer should not need a webhook. What must not happen is silence about
// it, so the absence is logged at WARN naming the consequence -- the same
// shape `runOpsRetention` uses, and for the same reason.
//
// In STAGING and PROD it is fatal, and that changed here (F-137). The old
// comment said "config.Validate refuses this in STAGING and PROD, so this
// branch is a developer's laptop rather than a deployment" -- and it was not
// true of the branch below it. `RuleAlertDestination` could only see the
// env:// REFERENCE, which render.yaml always writes, so the deployment that
// had never set NODAL_ALERT_WEBHOOK_URL arrived HERE, at ERROR-and-continue,
// and served healthy and unalerted. `config.ResolvableSecrets` now refuses that
// configuration at load; this is the second refusal, for the resolver the
// running process holds and the loader does not (aws-sm://), and for a
// destination that resolves to something unusable.
func newAlertDispatcher(ctx context.Context, cfg *config.Config, resolver config.Resolver, log *slog.Logger) (*alert.Dispatcher, error) {
	if cfg.Alert.WebhookURL.IsZero() {
		if cfg.Env.IsProductionLike() {
			return nil, fmt.Errorf("CP_ALERT_WEBHOOK_URL is not set: %s may not serve with nowhere for a "+
				"ledger-integrity violation to go", cfg.Env)
		}
		log.Warn("alerts are not delivered anywhere: CP_ALERT_WEBHOOK_URL is not set",
			"consequence", "a ledger-integrity violation is written to this service's log and to nothing else",
			"note", "STAGING and PROD refuse to start in this state")
		return nil, nil
	}
	raw, err := resolver.Resolve(ctx, cfg.Alert.WebhookURL)
	if err == nil && strings.TrimSpace(raw) == "" {
		err = errors.New("the reference resolves to an empty value")
	}
	if err != nil {
		if cfg.Env.IsProductionLike() {
			// The error names the reference, never the value: a webhook URL is
			// its own credential.
			return nil, fmt.Errorf("CP_ALERT_WEBHOOK_URL (%s) could not be resolved in %s: %w",
				cfg.Alert.WebhookURL.Redacted(), cfg.Env, err)
		}
		// Outside STAGING and PROD an unresolvable alert destination must not
		// stop a service from serving: the alternative to an alert is not an
		// outage. It is logged at ERROR because a deployment that meant to have
		// alerting and does not is exactly the state this finding is about.
		log.Error("alerts are not delivered anywhere: CP_ALERT_WEBHOOK_URL could not be resolved",
			"error", err.Error(),
			"consequence", "a ledger-integrity violation is written to this service's log and to nothing else")
		return nil, nil
	}
	host := "(unparseable)"
	if u, perr := url.Parse(raw); perr == nil && u.Host != "" {
		host = u.Host
	}

	timeout := cfg.Alert.Timeout
	if budget := cfg.API.ShutdownTimeout - alertDeliveryTimeoutHeadroom; budget > 0 && timeout > budget {
		// A delivery that outlives the shutdown budget it drains inside would
		// turn a clean stop into a kill. Trimmed rather than refused: a shorter
		// alert timeout is a smaller loss than a service that will not restart.
		log.Warn("alert delivery timeout trimmed to fit the shutdown budget",
			"configured", cfg.Alert.Timeout, "using", budget)
		timeout = budget
	}

	sink := alert.NewWebhookSink(raw, host, alert.Format(cfg.Alert.WebhookFormat), &http.Client{Timeout: timeout})
	d := alert.NewDispatcher(alert.Options{
		Sink: sink,
		// Validated to SEV1 or SEV2, so this cast is total.
		MinSeverity: alert.Severity(cfg.Alert.MinSeverity),
		Timeout:     timeout,
		Logger:      log,
	})
	log.Info("alerts are delivered", "destination", sink.Describe(), "min_severity", cfg.Alert.MinSeverity, "timeout", timeout)
	return d, nil
}

// attachAlertDispatcher joins the raise path to the dispatcher.
//
// The translation happens here rather than in either package: `alert` must not
// import `reconciliation` (it would depend on what raises) and `reconciliation`
// must not import `alert` (it would depend on where alerts go). The composition
// root is the only place that may know both.
//
// `alert.EventFrom` applies the field allowlist, so a field added to a Raise
// call site tomorrow does not silently start leaving the process.
func attachAlertDispatcher(m *reconciliation.Metrics, d *alert.Dispatcher, cfg *config.Config) {
	if m == nil || d == nil {
		return
	}
	m.OnAlert(func(a reconciliation.Alert) {
		d.Enqueue(alert.EventFrom(
			a.Name, alert.Severity(a.Severity), a.Detail, a.RecordID, a.Fields, a.At,
			string(cfg.Env), cfg.ServiceName,
		))
	})
}
