package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/verification"
)

// The verification expiry pass (D-085).
//
// Five minutes, the same cadence as runInternalVerification and for the same
// reason: this deployment runs one process (render.yaml), so periodic work runs
// here or nowhere, and nowhere is what left D-061 half-built.
//
// # What it is NOT for
//
// It is not the control that stops an expired verification paying somebody out.
// That is verification.Resolver, which reads `expires_at` itself and reports the
// base level for a profile whose window has elapsed whatever the state column
// says. D-061 chose that on purpose: "a sweep that moves an expired profile to
// EXPIRED will exist and will sometimes be late", and a payout decision that
// waited for a timer would be a payout decision with a race in it.
//
// What this fixes is the disagreement the resolver leaves behind. A profile
// reading VERIFIED while every surface treats the person as unverified is a
// support conversation nobody can win, and §20's EXPIRED state -- whose next
// step the profile view renders as REVERIFY -- is unreachable until something
// writes it.
//
// # Cost
//
// One indexed read per pass over compliance_profiles WHERE identity_state =
// 'VERIFIED' AND expires_at <= now(), which on a launch-tier database is
// nothing, and a short transaction per profile it finds. On a deployment whose
// oldest verification is under a year old it finds none, every time, forever --
// which is the normal case and is meant to be.
const verifyExpiryInterval = 5 * time.Minute

// runVerificationExpiry moves overdue verifications to EXPIRED for as long as
// this process is up.
func runVerificationExpiry(ctx context.Context, database *db.DB, svc *verification.Service, clk clock.Clock, log *slog.Logger) {
	if database == nil || svc == nil {
		return
	}
	log.Info("verification expiry sweep started",
		"interval", verifyExpiryInterval, "validity_window", verification.ValidityWindow)
	t := time.NewTicker(verifyExpiryInterval)
	defer t.Stop()
	for {
		// Once at start as well as on the tick, for the same reason as the
		// settlement sweep: a free instance that wakes, serves and spins down
		// would otherwise never run it at all.
		expireOverdueOnce(ctx, database, svc, clk, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// expireOverdueOnce is one pass. A failure is logged and the next tick tries
// again: the resolver is already reporting these people at their base level, so
// a late pass costs a stale state column and nothing else.
func expireOverdueOnce(ctx context.Context, database *db.DB, svc *verification.Service, clk clock.Clock, log *slog.Logger) {
	moved, err := svc.ExpireOverdue(ctx, database, clk.Now(), verification.SweepBatch)
	switch {
	case err != nil && ctx.Err() != nil:
		return // shutdown, not a failure
	case err != nil:
		log.ErrorContext(ctx, "verification expiry sweep failed",
			"error", err.Error(), "moved", moved,
			"consequence", "some profiles still read VERIFIED past their window; "+
				"the resolver already reports those people at their base level, so nothing they hold can leave")
	case moved > 0:
		log.InfoContext(ctx, "verification decisions expired", "moved", moved,
			"consequence", "these people are asked to verify again; it is not a rejection")
	}
}
