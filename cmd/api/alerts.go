package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
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
// is configured.
//
// A nil dispatcher is a working configuration, not a failure: LOCAL and TEST
// have no webhook and should not need one. What must not happen is silence
// about it, so the absence is logged at WARN naming the consequence -- the same
// shape `runOpsRetention` uses, and for the same reason. `config.Validate`
// refuses an empty destination in STAGING and PROD, so this branch is a
// developer's laptop rather than a deployment.
func newAlertDispatcher(ctx context.Context, cfg *config.Config, resolver config.Resolver, log *slog.Logger) *alert.Dispatcher {
	if cfg.Alert.WebhookURL.IsZero() {
		log.Warn("alerts are not delivered anywhere: CP_ALERT_WEBHOOK_URL is not set",
			"consequence", "a ledger-integrity violation is written to this service's log and to nothing else",
			"note", "config.Validate refuses this in STAGING and PROD")
		return nil
	}
	raw, err := resolver.Resolve(ctx, cfg.Alert.WebhookURL)
	if err != nil {
		// Not fatal. An unresolvable alert destination must not stop a service
		// from serving: the alternative to an alert is not an outage. It is
		// logged at ERROR because a deployment that meant to have alerting and
		// does not is exactly the state this finding is about.
		log.Error("alerts are not delivered anywhere: CP_ALERT_WEBHOOK_URL could not be resolved",
			"error", err.Error(),
			"consequence", "a ledger-integrity violation is written to this service's log and to nothing else")
		return nil
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
	return d
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
