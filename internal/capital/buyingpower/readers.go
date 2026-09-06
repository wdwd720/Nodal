package buyingpower

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
)

// KillSwitchState is an active emergency control that affects the account
// (PART 52). The real reader (Stage 3) maps kill_switches.kind to the two
// flags; the engine only consumes the flags.
type KillSwitchState struct {
	Kind             string
	ScopeID          string
	Reason           string
	BlocksNewRisk    bool // GLOBAL_NEW_RISK_KILL, ACCOUNT_FREEZE, ...
	BlocksWithdrawal bool // WITHDRAWALS_DISABLE, ...
}

// KillSwitchReader lists the active kill switches relevant to an account.
type KillSwitchReader interface {
	Active(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]KillSwitchState, error)
}

// ReconciliationBlock is an open reconciliation record that blocks new
// risk for the account (PART 50: blocks_new_risk).
type ReconciliationBlock struct {
	RecordID string
	Kind     string
	Detail   string
}

// ReconciliationBlockReader lists open reconciliation blocks for an account.
type ReconciliationBlockReader interface {
	Blocks(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]ReconciliationBlock, error)
}
