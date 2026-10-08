package transfers

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/notification"
)

// Notice is what the recipient of a transfer is told. It never carries the
// memo: the memo is read through the transfer itself, by its parties.
type Notice struct {
	TransferID         TransferID
	RecipientAccountID accounts.AccountID
	RecipientUserID    accounts.UserID
	AssetID            assets.AssetID
	AssetSymbol        string
	AssetDecimals      uint8
	Quantity           money.Quantity
	CorrelationID      string
}

// Notifier tells a recipient about a transfer, inside the transaction that
// moved the value, so a notification can never describe something that did
// not happen. An error fails the transfer.
type Notifier interface {
	TransferReceived(ctx context.Context, tx pgx.Tx, n Notice) error
	TransferReversed(ctx context.Context, tx pgx.Tx, n Notice) error
}

// NotificationNotifier writes in-app notifications through
// internal/notification, as TRANSFER_RECEIVED and TRANSFER_REVERSED (00733).
// The data payload names the transfer event and marks the value simulated;
// the dedup key makes a replay notify exactly once.
type NotificationNotifier struct {
	repo *notification.Repository
}

// NewNotificationNotifier wires a NotificationNotifier.
func NewNotificationNotifier(repo *notification.Repository) *NotificationNotifier {
	if repo == nil {
		panic("transfers: NewNotificationNotifier requires a notification repository")
	}
	return &NotificationNotifier{repo: repo}
}

// TransferReceived implements Notifier.
func (n *NotificationNotifier) TransferReceived(ctx context.Context, tx pgx.Tx, nt Notice) error {
	return n.write(ctx, tx, nt, notification.KindTransferReceived, "p2p_transfer_received", "p2p-received:",
		"Simulated transfer received",
		"You received "+nt.Quantity.ToDecimalString(nt.AssetDecimals)+" "+nt.AssetSymbol+
			" of simulated value from another customer. Simulated value only: nothing real moved.")
}

// TransferReversed implements Notifier.
func (n *NotificationNotifier) TransferReversed(ctx context.Context, tx pgx.Tx, nt Notice) error {
	return n.write(ctx, tx, nt, notification.KindTransferReversed, "p2p_transfer_reversed", "p2p-reversed:",
		"Simulated transfer reversed",
		"A simulated transfer of "+nt.Quantity.ToDecimalString(nt.AssetDecimals)+" "+nt.AssetSymbol+
			" you received was reversed by an operator. Simulated value only: nothing real moved.")
}

func (n *NotificationNotifier) write(ctx context.Context, tx pgx.Tx, nt Notice, kind notification.Kind, eventName, dedupPrefix, title, body string) error {
	data, err := json.Marshal(map[string]string{
		"event":       eventName,
		"transfer_id": nt.TransferID.String(),
		"asset_id":    nt.AssetID.String(),
		"quantity":    nt.Quantity.String(),
		"simulated":   "true",
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "transfers: encode notification data")
	}
	acct := nt.RecipientAccountID
	_, _, err = n.repo.Create(ctx, tx, notification.Notification{
		UserID: nt.RecipientUserID, AccountID: &acct, Kind: kind, Severity: notification.SeverityInfo,
		Title: title, Body: body, ResourceType: "internal_transfer", ResourceID: nt.TransferID.String(),
		Data: data, DedupKey: dedupPrefix + nt.TransferID.String(), CorrelationID: nt.CorrelationID,
	})
	if err != nil {
		return dbErr("notify recipient", err)
	}
	return nil
}
