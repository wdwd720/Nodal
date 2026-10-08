package transfers

import (
	"time"

	"github.com/nodal/controlplane/internal/money"
)

// decisionEvidence is the canonical evidence document of one transfer
// decision, archived write-once under "advanced/p2p/<transfer id>.json" in the
// decision's own transaction. It names every input the decision depended on
// and every rule it applied, so an auditor can recompute it. Money renders as
// strings; the memo appears only as its digest.
type decisionEvidence struct {
	Schema        string   `json:"schema"`
	TransferID    string   `json:"transfer_id"`
	IntentID      string   `json:"intent_id"`
	Outcome       string   `json:"outcome"`
	RejectionCode string   `json:"rejection_code"`
	ReasonCodes   []string `json:"reason_codes"`
	Mode          string   `json:"mode"`

	SenderAccountID    string `json:"sender_account_id"`
	SenderUserID       string `json:"sender_user_id"`
	RecipientAccountID string `json:"recipient_account_id"`
	RecipientHandle    string `json:"recipient_handle"`
	MemoSHA256         string `json:"memo_sha256"`

	AssetID       string         `json:"asset_id"`
	AssetSymbol   string         `json:"asset_symbol"`
	AssetDecimals int            `json:"asset_decimals"`
	Quantity      money.Quantity `json:"quantity"`
	NotionalUSD   money.USD      `json:"notional_usd"`
	Valuation     Quote          `json:"valuation"`

	Policy         Policy          `json:"policy"`
	PolicyHash     string          `json:"policy_hash"`
	PolicyDecision *PolicyDecision `json:"policy_decision"`
	StepUpRequired bool            `json:"step_up_required"`

	Route  *routeEvidence `json:"route"`
	Risk   *riskEvidence  `json:"risk"`
	Screen *ScreenResult  `json:"screen"`
	// ScreenError is why the screen could not answer, when it could not.
	ScreenError string `json:"screen_error"`

	ReservationID        string    `json:"reservation_id"`
	JournalTransactionID string    `json:"journal_transaction_id"`
	DecidedAt            time.Time `json:"decided_at"`
}

type routeEvidence struct {
	Permitted bool     `json:"permitted"`
	Reasons   []string `json:"reasons"`
	Product   string   `json:"product"`
	Executor  string   `json:"executor"`
	Domain    string   `json:"value_domain"`
}

type riskEvidence struct {
	DecisionID       string   `json:"decision_id"`
	DecisionHash     string   `json:"decision_hash"`
	Verdict          string   `json:"verdict"`
	ReasonCodes      []string `json:"reason_codes"`
	MissingSections  []string `json:"missing_sections"`
	PolicyVersion    string   `json:"policy_version"`
	PolicyHash       string   `json:"policy_hash"`
	EvaluatorVersion string   `json:"evaluator_version"`
	InputHash        string   `json:"input_hash"`
}

func (s *Service) evidence(st *sendState, outcome Status, codes []string) decisionEvidence {
	if codes == nil {
		codes = []string{}
	}
	doc := decisionEvidence{
		Schema: "nodal.transfers.decision/v1", TransferID: st.t.ID.String(), IntentID: st.t.IntentID.String(),
		Outcome: string(outcome), ReasonCodes: codes, Mode: string(st.t.Mode),
		SenderAccountID: st.t.SenderAccountID.String(), SenderUserID: st.t.SenderUserID.String(),
		RecipientAccountID: st.t.RecipientAccountID.String(), RecipientHandle: st.t.RecipientHandle,
		MemoSHA256: MemoDigest(st.t.Memo),
		AssetID:    st.asset.ID.String(), AssetSymbol: st.asset.Symbol, AssetDecimals: int(st.asset.Decimals),
		Quantity: st.t.Quantity, NotionalUSD: st.t.NotionalUSD, Valuation: st.quote,
		Policy: s.policy, PolicyHash: s.policyHash, PolicyDecision: st.policy, StepUpRequired: st.stepUp,
		Screen: st.screen, ScreenError: st.screenErr, DecidedAt: st.now,
	}
	if len(codes) > 0 {
		doc.RejectionCode = codes[0]
	}
	if st.route != nil {
		reasons := st.route.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		doc.Route = &routeEvidence{
			Permitted: st.route.Permitted, Reasons: reasons, Product: st.route.Product,
			Executor: string(st.route.Executor), Domain: string(st.route.ValueDomain),
		}
	}
	if st.decision != nil {
		r := &riskEvidence{
			DecisionHash: st.decision.Hash, Verdict: string(st.decision.Verdict), ReasonCodes: st.decision.ReasonCodes,
			MissingSections: st.decision.MissingSections, PolicyVersion: st.decision.PolicyVersion,
			PolicyHash: st.decision.PolicyHash, EvaluatorVersion: st.decision.EvaluatorVersion, InputHash: st.decision.InputHash,
		}
		if r.ReasonCodes == nil {
			r.ReasonCodes = []string{}
		}
		if st.riskID != nil {
			r.DecisionID = st.riskID.String()
		}
		doc.Risk = r
	}
	if st.rsv != nil {
		doc.ReservationID = st.rsv.ID.String()
	}
	if st.journal != nil {
		doc.JournalTransactionID = st.journal.String()
	}
	return doc
}

// reversalEvidence is the canonical evidence of one reversal.
type reversalEvidence struct {
	Schema                   string         `json:"schema"`
	TransferID               string         `json:"transfer_id"`
	IntentID                 string         `json:"intent_id"`
	OriginalJournal          string         `json:"original_journal_transaction_id"`
	ReversalJournal          string         `json:"reversal_journal_transaction_id"`
	EffectID                 string         `json:"effect_id"`
	ReasonCode               string         `json:"reason_code"`
	Reason                   string         `json:"reason"`
	Operator                 string         `json:"operator"`
	Sender                   string         `json:"sender_account_id"`
	Recipient                string         `json:"recipient_account_id"`
	Asset                    string         `json:"asset_id"`
	Quantity                 money.Quantity `json:"quantity"`
	RecipientAvailableBefore money.Quantity `json:"recipient_available_before"`
	ReservationID            string         `json:"reservation_id"`
	ReversedAt               time.Time      `json:"reversed_at"`
}
