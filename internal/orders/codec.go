package orders

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/matching"
)

// The journal stores every applied command as canonical bytes: the JSON
// encoding of commandDoc, whose fields are always present, in declaration
// order, enums as their engine numbers and integers as integers (Go's
// encoding/json writes an int64 exactly and reads it back exactly into an
// int64). Decoding is strict: unknown fields are refused and the decoded
// command must re-encode to exactly the stored bytes, so one command has one
// encoding and its sha256 identifies it.

// maxCommandBytes bounds a stored command; the database checks it too.
const maxCommandBytes = 8192

type commandDoc struct {
	Kind                uint8  `json:"kind"`
	Timestamp           int64  `json:"timestamp_unix_ns"`
	OrderID             string `json:"order_id"`
	ClientOrderID       string `json:"client_order_id"`
	OwnerID             string `json:"owner_id"`
	Side                uint8  `json:"side"`
	Type                uint8  `json:"type"`
	TIF                 uint8  `json:"tif"`
	STP                 uint8  `json:"stp"`
	PriceTicks          int64  `json:"price_ticks"`
	QtyLots             int64  `json:"qty_lots"`
	PostOnly            bool   `json:"post_only"`
	ReduceOnly          bool   `json:"reduce_only"`
	ReduceOnlyLimitLots int64  `json:"reduce_only_limit_lots"`
}

// EncodeCommand returns the canonical bytes of a command.
func EncodeCommand(cmd matching.Command) ([]byte, error) {
	var d commandDoc
	switch c := cmd.(type) {
	case matching.NewOrder:
		d = commandDoc{
			Kind: uint8(matching.CommandNewOrder), Timestamp: c.Timestamp, OrderID: c.OrderID, ClientOrderID: c.ClientOrderID,
			OwnerID: c.OwnerID, Side: uint8(c.Side), Type: uint8(c.Type), TIF: uint8(c.TIF), STP: uint8(c.STP),
			PriceTicks: c.PriceTicks, QtyLots: c.QtyLots, PostOnly: c.PostOnly, ReduceOnly: c.ReduceOnly,
			ReduceOnlyLimitLots: c.ReduceOnlyLimitLots,
		}
	case matching.Cancel:
		d = commandDoc{Kind: uint8(matching.CommandCancel), Timestamp: c.Timestamp, OrderID: c.OrderID, OwnerID: c.OwnerID}
	case matching.Replace:
		d = commandDoc{Kind: uint8(matching.CommandReplace), Timestamp: c.Timestamp, OrderID: c.OrderID, OwnerID: c.OwnerID,
			PriceTicks: c.NewPriceTicks, QtyLots: c.NewQtyLots}
	default:
		return nil, errs.New(errs.CodeValidationFailed, "orders: not a matching command")
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "orders: encode command")
	}
	if len(b) > maxCommandBytes {
		return nil, errs.New(errs.CodeValidationFailed, "orders: command too large to journal")
	}
	return b, nil
}

// DecodeCommand rebuilds a command from its canonical bytes. It never panics;
// anything that is not exactly the canonical encoding of a command is refused.
func DecodeCommand(b []byte) (matching.Command, error) {
	if len(b) == 0 || len(b) > maxCommandBytes {
		return nil, errs.New(errs.CodeValidationFailed, "orders: stored command has an invalid length")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var d commandDoc
	if err := dec.Decode(&d); err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "orders: stored command is not a command")
	}
	var cmd matching.Command
	switch matching.CommandKind(d.Kind) {
	case matching.CommandNewOrder:
		cmd = matching.NewOrder{
			Timestamp: d.Timestamp, OrderID: d.OrderID, ClientOrderID: d.ClientOrderID, OwnerID: d.OwnerID,
			Side: matching.Side(d.Side), Type: matching.OrderType(d.Type), TIF: matching.TimeInForce(d.TIF),
			STP: matching.STPMode(d.STP), PriceTicks: d.PriceTicks, QtyLots: d.QtyLots, PostOnly: d.PostOnly,
			ReduceOnly: d.ReduceOnly, ReduceOnlyLimitLots: d.ReduceOnlyLimitLots,
		}
	case matching.CommandCancel:
		cmd = matching.Cancel{Timestamp: d.Timestamp, OrderID: d.OrderID, OwnerID: d.OwnerID}
	case matching.CommandReplace:
		cmd = matching.Replace{Timestamp: d.Timestamp, OrderID: d.OrderID, OwnerID: d.OwnerID,
			NewPriceTicks: d.PriceTicks, NewQtyLots: d.QtyLots}
	default:
		return nil, errs.New(errs.CodeValidationFailed, "orders: stored command has an unknown kind")
	}
	again, err := EncodeCommand(cmd)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(again, b) {
		return nil, errs.New(errs.CodeValidationFailed, "orders: stored command is not in canonical form")
	}
	return cmd, nil
}

// eventDoc has exactly the fields of matching.Event, so an event converts to
// it and back without loss; its JSON is the event's canonical encoding.
type eventDoc struct {
	Seq        int64                `json:"seq"`
	CommandSeq int64                `json:"command_seq"`
	Timestamp  int64                `json:"timestamp_unix_ns"`
	Command    matching.CommandKind `json:"command"`
	Type       matching.EventType   `json:"type"`

	OrderID       string `json:"order_id"`
	ClientOrderID string `json:"client_order_id"`
	OwnerID       string `json:"owner_id"`

	Side        matching.Side        `json:"side"`
	OrderType   matching.OrderType   `json:"order_type"`
	TimeInForce matching.TimeInForce `json:"tif"`
	STP         matching.STPMode     `json:"stp"`
	PostOnly    bool                 `json:"post_only"`
	ReduceOnly  bool                 `json:"reduce_only"`

	PriceTicks    int64 `json:"price_ticks"`
	QtyLots       int64 `json:"qty_lots"`
	FilledLots    int64 `json:"filled_lots"`
	RemainingLots int64 `json:"remaining_lots"`

	RejectReason matching.RejectReason `json:"reject_reason"`
	CancelReason matching.CancelReason `json:"cancel_reason"`

	PrevPriceTicks int64 `json:"prev_price_ticks"`
	PrevQtyLots    int64 `json:"prev_qty_lots"`
	PriorityKept   bool  `json:"priority_kept"`

	Priority int64 `json:"priority"`

	TradeSeq      int64         `json:"trade_seq"`
	MakerOrderID  string        `json:"maker_order_id"`
	MakerOwnerID  string        `json:"maker_owner_id"`
	TakerOrderID  string        `json:"taker_order_id"`
	TakerOwnerID  string        `json:"taker_owner_id"`
	AggressorSide matching.Side `json:"aggressor_side"`

	LevelQtyLots int64 `json:"level_qty_lots"`
}

// EncodeEvent returns the canonical bytes of an engine event.
func EncodeEvent(e matching.Event) ([]byte, error) {
	b, err := json.Marshal(eventDoc(e))
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "orders: encode event")
	}
	return b, nil
}

// DecodeEvent rebuilds an engine event from its canonical bytes, refusing
// anything that does not re-encode to exactly those bytes.
func DecodeEvent(b []byte) (matching.Event, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var d eventDoc
	if err := dec.Decode(&d); err != nil {
		return matching.Event{}, errs.Wrap(err, errs.CodeValidationFailed, "orders: stored event is not an event")
	}
	e := matching.Event(d)
	again, err := EncodeEvent(e)
	if err != nil {
		return matching.Event{}, err
	}
	if !bytes.Equal(again, b) {
		return matching.Event{}, errs.New(errs.CodeValidationFailed, "orders: stored event is not in canonical form")
	}
	return e, nil
}

func sha(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
