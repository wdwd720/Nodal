package devnethost

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

// EventKind names one journaled host operation. Every call that can change
// the state of a chain -- including a fault that changes only what the next
// submission will do -- is one event, so replaying the events in order
// reproduces the network exactly.
type EventKind string

// Event kinds.
const (
	EventGenesis        EventKind = "GENESIS"
	EventCreateMint     EventKind = "CREATE_MINT"
	EventAirdrop        EventKind = "AIRDROP"
	EventSetMintFrozen  EventKind = "SET_MINT_FROZEN"
	EventSubmit         EventKind = "SUBMIT"
	EventProduceBlocks  EventKind = "PRODUCE_BLOCKS"
	EventDropNext       EventKind = "DROP_NEXT"
	EventTimeoutButLand EventKind = "TIMEOUT_BUT_LAND"
	EventTimeoutAndDrop EventKind = "TIMEOUT_AND_DROP"
	EventDelayInclusion EventKind = "DELAY_INCLUSION"
	EventClearFaults    EventKind = "CLEAR_FAULTS"
	EventReorg          EventKind = "REORG"
)

var allEventKinds = []EventKind{
	EventGenesis, EventCreateMint, EventAirdrop, EventSetMintFrozen, EventSubmit, EventProduceBlocks,
	EventDropNext, EventTimeoutButLand, EventTimeoutAndDrop, EventDelayInclusion, EventClearFaults, EventReorg,
}

// AllEventKinds returns every event kind in declaration order (a copy). It
// is the Go twin of the CHECK devnet_events_kind_check.
func AllEventKinds() []EventKind { return append([]EventKind(nil), allEventKinds...) }

// Valid reports whether k is a declared kind.
func (k EventKind) Valid() bool {
	for _, x := range allEventKinds {
		if x == k {
			return true
		}
	}
	return false
}

// Outcome is what a journaled call did.
type Outcome string

// Outcomes.
const (
	// OutcomeApplied: an operator call or fault that took effect.
	OutcomeApplied Outcome = "APPLIED"
	// OutcomeAccepted: a submission the chain accepted (or accepted and
	// dropped under an armed DropNext; the event records the call, and the
	// replay reproduces the drop).
	OutcomeAccepted Outcome = "ACCEPTED"
	// OutcomeTimeout: a submission that consumed an armed timeout fault and
	// returned SUBMISSION_STATE_UNKNOWN.
	OutcomeTimeout Outcome = "TIMEOUT"
)

var allOutcomes = []Outcome{OutcomeApplied, OutcomeAccepted, OutcomeTimeout}

// AllOutcomes returns every outcome (a copy). It is the Go twin of the CHECK
// devnet_events_outcome_check.
func AllOutcomes() []Outcome { return append([]Outcome(nil), allOutcomes...) }

// SubmitOutcomes returns the outcomes a SUBMIT event carries (a copy). It is
// the Go twin of the CHECK devnet_events_submit_outcome.
func SubmitOutcomes() []Outcome { return []Outcome{OutcomeAccepted, OutcomeTimeout} }

// Event is one devnet_events row.
type Event struct {
	Network string
	Seq     uint64
	Kind    EventKind
	// Chain is empty for GENESIS.
	Chain devnet.ChainID
	// Args is the canonical JSON document of the call's arguments and, where
	// the call returned something replay must reproduce, its result.
	Args json.RawMessage
	// RawTx and TxID are set for SUBMIT only: the exact submitted bytes and
	// the transaction id the chain returned.
	RawTx []byte
	TxID  devnet.TxID
	// Outcome is what the call did.
	Outcome Outcome
	// StateHash is the network's StateHash after the call. Replay recomputes
	// it after re-applying the call and refuses to continue on a difference.
	StateHash devnet.Hash
	// PrevHash is the previous event's EventHash (zero for GENESIS) and
	// EventHash commits to this row and to PrevHash, so the history is a
	// hash chain a replay verifies end to end.
	PrevHash  [32]byte
	EventHash [32]byte
	// OccurredAt is when the call was made; for SUBMIT it is the submission
	// time. Microsecond precision, the database's.
	OccurredAt time.Time
}

// Argument documents. Integers that can exceed 2^53 are decimal strings.
type (
	createMintArgs struct {
		Address   string `json:"address"`
		Decimals  int    `json:"decimals"`
		Authority string `json:"authority"`
		Frozen    bool   `json:"frozen"`
	}
	airdropArgs struct {
		To       string `json:"to"`
		Lamports string `json:"lamports"`
	}
	setMintFrozenArgs struct {
		Mint   string `json:"mint"`
		Frozen bool   `json:"frozen"`
	}
	submitArgs struct {
		TxID string `json:"tx_id"`
	}
	produceArgs struct {
		Blocks    string `json:"blocks"`
		TipHeight string `json:"tip_height"`
		TipHash   string `json:"tip_hash"`
	}
	countArgs struct {
		N string `json:"n"`
	}
	noArgs    struct{}
	reorgArgs struct {
		Depth   string `json:"depth"`
		Mode    string `json:"mode"`
		Removed string `json:"removed"`
	}
)

// Reorg modes as they are written in events.
const (
	reorgRequeue = "REQUEUE"
	reorgDrop    = "DROP"
)

func reorgModeName(m devnet.ReorgMode) (string, bool) {
	switch m {
	case devnet.ReorgRequeue:
		return reorgRequeue, true
	case devnet.ReorgDrop:
		return reorgDrop, true
	}
	return "", false
}

func reorgModeOf(name string) (devnet.ReorgMode, bool) {
	switch name {
	case reorgRequeue:
		return devnet.ReorgRequeue, true
	case reorgDrop:
		return devnet.ReorgDrop, true
	}
	return 0, false
}

// canonicalArgs renders an argument document canonically.
func canonicalArgs(v any) (json.RawMessage, error) {
	b, err := audit.CanonicalJSON(v)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "devnethost: event arguments are not canonically encodable")
	}
	return b, nil
}

// hashedEvent is the document an event's hash covers.
type hashedEvent struct {
	Network    string          `json:"network"`
	Seq        string          `json:"seq"`
	Kind       string          `json:"kind"`
	Chain      string          `json:"chain"`
	Args       json.RawMessage `json:"args"`
	RawTx      string          `json:"raw_tx"`
	TxID       string          `json:"tx_id"`
	Outcome    string          `json:"outcome"`
	StateHash  string          `json:"state_hash"`
	PrevHash   string          `json:"prev_hash"`
	OccurredAt time.Time       `json:"occurred_at"`
}

// computeHash returns the EventHash e must carry: SHA-256 of a domain tag and
// the canonical JSON of every column but the hash itself.
func (e Event) computeHash() ([32]byte, error) {
	doc := hashedEvent{
		Network: e.Network, Seq: strconv.FormatUint(e.Seq, 10), Kind: string(e.Kind), Chain: string(e.Chain),
		Args: e.Args, RawTx: hex.EncodeToString(e.RawTx), TxID: string(e.TxID), Outcome: string(e.Outcome),
		StateHash: e.StateHash.String(), PrevHash: hex.EncodeToString(e.PrevHash[:]), OccurredAt: e.OccurredAt.UTC(),
	}
	if len(doc.Args) == 0 {
		doc.Args = json.RawMessage(`{}`)
	}
	b, err := audit.CanonicalJSON(doc)
	if err != nil {
		return [32]byte{}, errs.Wrap(err, errs.CodeInternal, "devnethost: event is not canonically encodable")
	}
	return sha256.Sum256(append([]byte(eventHashDomain), b...)), nil
}

const eventHashDomain = "nodal-devnethost/event/v1\x00"

// decodeArgs decodes an event's argument document strictly: unknown fields
// and trailing data are refused. It never panics on any input.
func decodeArgs(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "devnethost: event arguments do not decode").WithField("reason", ReasonReplayDiverged)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errs.New(errs.CodeInternal, "devnethost: trailing data after event arguments").WithField("reason", ReasonReplayDiverged)
	}
	return nil
}

func parseU64(s, field string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || strconv.FormatUint(v, 10) != s {
		return 0, diverged("event field " + field + " is not a canonical unsigned integer")
	}
	return v, nil
}

func parseKey(s, field string) (devnet.PublicKey, error) {
	k, err := devnet.ParsePublicKey(s)
	if err != nil {
		return devnet.PublicKey{}, diverged("event field " + field + " is not a canonical public key")
	}
	return k, nil
}

// replayEvent re-applies a non-GENESIS event to net and checks that every
// result the event recorded is reproduced. It changes net only through the
// devnet calls the event names.
func replayEvent(net *devnet.Network, ev Event) error {
	chain, err := net.Chain(ev.Chain)
	if err != nil {
		return diverged("event names chain " + string(ev.Chain) + " which the network does not have")
	}
	switch ev.Kind {
	case EventCreateMint:
		var a createMintArgs
		if err := decodeArgs(ev.Args, &a); err != nil {
			return err
		}
		addr, err := parseKey(a.Address, "address")
		if err != nil {
			return err
		}
		auth, err := parseKey(a.Authority, "authority")
		if err != nil {
			return err
		}
		if a.Decimals < 0 || a.Decimals > devnet.MaxDecimals {
			return diverged("event decimals out of range")
		}
		if err := chain.CreateMint(devnet.MintConfig{Address: addr, Decimals: uint8(a.Decimals), Authority: auth, Frozen: a.Frozen}); err != nil {
			return diverged("CREATE_MINT no longer applies: " + err.Error())
		}
	case EventAirdrop:
		var a airdropArgs
		if err := decodeArgs(ev.Args, &a); err != nil {
			return err
		}
		to, err := parseKey(a.To, "to")
		if err != nil {
			return err
		}
		lamports, err := parseU64(a.Lamports, "lamports")
		if err != nil {
			return err
		}
		if err := chain.Airdrop(to, lamports); err != nil {
			return diverged("AIRDROP no longer applies: " + err.Error())
		}
	case EventSetMintFrozen:
		var a setMintFrozenArgs
		if err := decodeArgs(ev.Args, &a); err != nil {
			return err
		}
		mint, err := parseKey(a.Mint, "mint")
		if err != nil {
			return err
		}
		if err := chain.SetMintFrozen(mint, a.Frozen); err != nil {
			return diverged("SET_MINT_FROZEN no longer applies: " + err.Error())
		}
	case EventSubmit:
		var a submitArgs
		if err := decodeArgs(ev.Args, &a); err != nil {
			return err
		}
		if a.TxID != string(ev.TxID) {
			return diverged("SUBMIT arguments disagree with the row's tx id")
		}
		id, err := chain.Submit(ev.RawTx)
		switch ev.Outcome {
		case OutcomeAccepted:
			if err != nil || id != ev.TxID {
				return diverged("SUBMIT was accepted originally and is not now")
			}
		case OutcomeTimeout:
			if devnet.ReasonOf(err) != devnet.CodeSubmissionTimeout || id != ev.TxID {
				return diverged("SUBMIT timed out originally and does not now")
			}
		default:
			return diverged("SUBMIT carries outcome " + string(ev.Outcome))
		}
	case EventProduceBlocks:
		var a produceArgs
		if err := decodeArgs(ev.Args, &a); err != nil {
			return err
		}
		n, err := parseU64(a.Blocks, "blocks")
		if err != nil {
			return err
		}
		if n == 0 || n > MaxBlocksPerCall {
			return diverged("PRODUCE_BLOCKS count out of range")
		}
		tip := chain.ProduceBlocks(n)
		if strconv.FormatUint(tip.Height, 10) != a.TipHeight || tip.Hash.String() != a.TipHash {
			return diverged("PRODUCE_BLOCKS reached a different tip")
		}
	case EventDropNext:
		var a countArgs
		if err := decodeArgs(ev.Args, &a); err != nil {
			return err
		}
		n, err := parseU64(a.N, "n")
		if err != nil {
			return err
		}
		chain.DropNext(n)
	case EventTimeoutButLand:
		if err := decodeArgs(ev.Args, &noArgs{}); err != nil {
			return err
		}
		chain.TimeoutButLand()
	case EventTimeoutAndDrop:
		if err := decodeArgs(ev.Args, &noArgs{}); err != nil {
			return err
		}
		chain.TimeoutAndDrop()
	case EventDelayInclusion:
		var a countArgs
		if err := decodeArgs(ev.Args, &a); err != nil {
			return err
		}
		n, err := parseU64(a.N, "n")
		if err != nil {
			return err
		}
		chain.DelayInclusion(n)
	case EventClearFaults:
		if err := decodeArgs(ev.Args, &noArgs{}); err != nil {
			return err
		}
		chain.ClearFaults()
	case EventReorg:
		var a reorgArgs
		if err := decodeArgs(ev.Args, &a); err != nil {
			return err
		}
		depth, err := parseU64(a.Depth, "depth")
		if err != nil {
			return err
		}
		mode, ok := reorgModeOf(a.Mode)
		if !ok {
			return diverged("REORG mode is unknown")
		}
		res, err := chain.Reorg(depth, mode)
		if err != nil || strconv.FormatUint(res.Removed, 10) != a.Removed {
			return diverged("REORG removed a different number of blocks")
		}
	default:
		return diverged("unknown event kind " + string(ev.Kind))
	}
	if ev.Kind != EventSubmit && ev.Outcome != OutcomeApplied {
		return diverged(string(ev.Kind) + " carries outcome " + string(ev.Outcome))
	}
	return nil
}
