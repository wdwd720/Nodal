package httpapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/killswitch"
)

// The executor table: what "execute" means for an approved administrative
// action, in the one place that knows both the action table and the domains.
//
// Most kinds deliberately have no executor. An approval is not always a
// command: CAPABILITY_GATE_APPROVE, RECONCILIATION_RESOLVE_MATERIAL,
// LEDGER_CORRECTION and WITHDRAWAL_APPROVE are *quoted* by the domain
// endpoint that does the work, which re-verifies the approval id against its
// own record (admin.VerifyApproved) before acting. Executing them here would
// be a second, weaker path to the same effect. The kinds below are the ones
// whose effect has no other home, and every other kind keeps answering 422
// UNSUPPORTED rather than reporting a success that never happened.

// AdminExecutors returns the executors this composition can honor. A nil
// dependency leaves its kind unregistered rather than half-wired.
func AdminExecutors(adminSvc *admin.Service, sessions *auth.Manager, kills *killswitch.Controller) map[admin.Kind]admin.ExecFunc {
	out := make(map[admin.Kind]admin.ExecFunc, 2)
	if adminSvc != nil && sessions != nil {
		out[admin.KindBreakGlassGrant] = BreakGlassGrantExecutor(admin.NewBreakGlass(adminSvc), sessions)
	}
	if kills != nil {
		out[admin.KindKillSwitchRelease] = KillSwitchReleaseExecutor(kills)
	}
	return out
}

// BreakGlassGrantExecutor executes an approved BREAK_GLASS_GRANT: it records
// the grant on the admin audit stream (internal/admin) and then writes the
// elevation onto the grantee's live sessions, both inside the savepoint
// Execute opens. If the elevation cannot be applied the grant is not
// recorded either — an executed grant that elevated nobody would read as
// authority that does not exist.
//
// It grants nothing by itself. The elevation it writes is only ever the one
// two other people approved: the grantee, the scope and the duration all
// come from the params whose hash Execute has already re-verified, and the
// duration is bounded by admin.MaxBreakGlassDuration at proposal time and
// again by auth.MaxBreakGlassElevation here.
func BreakGlassGrantExecutor(bg *admin.BreakGlass, sessions *auth.Manager) admin.ExecFunc {
	record := bg.Executor()
	return func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error) {
		result, err := record(ctx, tx, params)
		if err != nil {
			return nil, err
		}
		grant, err := admin.ParseGrant(result)
		if err != nil {
			return nil, err
		}
		elevated, err := sessions.Elevate(ctx, tx, grant.UserID, grant.ExpiresAt)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "the break-glass elevation could not be applied")
		}
		if elevated == 0 {
			// Fail rather than record a grant nobody holds: the operator
			// must be signed in when the grant is executed, and a FAILED
			// action says so where a silent no-op would not.
			return nil, errs.New(errs.CodeInvalidStateTransition,
				"the grantee has no live operator session to elevate; they must sign in before the grant is executed").
				WithField("user_id", grant.UserID)
		}
		return result, nil
	}
}

// killSwitchReleaseResult is the execution_result of a KILL_SWITCH_RELEASE.
type killSwitchReleaseResult struct {
	SwitchID   string     `json:"switch_id"`
	Kind       string     `json:"kind"`
	Scope      string     `json:"scope"`
	Severity   string     `json:"severity"`
	Active     bool       `json:"active"`
	ReleasedBy string     `json:"released_by"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	ApprovalID string     `json:"approval_id"`
}

// KillSwitchReleaseExecutor executes an approved KILL_SWITCH_RELEASE by
// releasing exactly the switch the action names — the (kind, scope) is read
// back out of the stored target_id, never from the request — citing the
// action itself as the approval.
//
// It weakens nothing. internal/killswitch still demands kill:release (which
// only a live break-glass elevation confers), a step-up within its own
// window, and an approval that verifies against this kind and target with a
// distinct approver. The executing principal only reaches Execute at all by
// holding the kind's propose or approve permission; the release then refuses
// them unless they hold kill:release too.
func KillSwitchReleaseExecutor(ctl *killswitch.Controller) admin.ExecFunc {
	return func(ctx context.Context, tx pgx.Tx, _ json.RawMessage) (json.RawMessage, error) {
		action, ok := admin.ExecutingAction(ctx)
		if !ok {
			return nil, errs.New(errs.CodeInternal, "the kill-switch executor runs only inside Execute")
		}
		if action.Kind != admin.KindKillSwitchRelease {
			return nil, errs.Newf(errs.CodeInvalidStateTransition,
				"action %s is not a kill-switch release", action.Kind)
		}
		kind, scope, err := killswitch.ParseReleaseTargetID(action.TargetID)
		if err != nil {
			return nil, err
		}
		approval := action.ID.String()
		sw, err := ctl.Release(ctx, tx, kind, scope, action.Reason, &approval)
		if err != nil {
			return nil, err
		}
		out, err := json.Marshal(killSwitchReleaseResult{
			SwitchID: sw.ID.String(), Kind: string(sw.Kind), Scope: sw.ScopeID,
			Severity: string(sw.Severity), Active: sw.Active,
			ReleasedBy: sw.ReleasedBy, ReleasedAt: timePtr(sw.ReleasedAt), ApprovalID: sw.ReleaseApprovalID,
		})
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "encode kill switch release result")
		}
		return out, nil
	}
}
