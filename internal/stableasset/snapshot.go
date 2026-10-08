package stableasset

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

const (
	snapshotVersion = "nodal-sandbox/stable-reserve-snapshot/v1"
	// SnapshotDisclaimer is part of every snapshot body, so no copy of one
	// can be presented without it.
	SnapshotDisclaimer = "Machine-verifiable snapshot of SIMULATED sandbox books and a simulated chain. " +
		"It is not an attestation, an audit, or a statement about any real reserve."
	snapshotSignatureDomain = "nodal-sandbox/stable-reserve-snapshot/signature/v1\x00"
)

// SnapshotReserve is one reserve asset in a snapshot.
type SnapshotReserve struct {
	AssetID   string    `json:"asset_id"`
	Kind      string    `json:"kind"`
	Balance   string    `json:"balance"`
	Decimals  string    `json:"decimals"`
	FactorBPS string    `json:"factor_bps"`
	Price     *PriceDoc `json:"price"`
	Eligible  string    `json:"eligible"`
}

// CompositionLine is the collateral composition: one reserve kind's share of
// the eligible reserve.
type CompositionLine struct {
	Kind     string `json:"kind"`
	Eligible string `json:"eligible"`
	ShareBPS string `json:"share_bps"`
}

// ReserveSnapshot is the body of a reserve and supply snapshot: what the
// books and the chain said at one instant, linked to the snapshot before it.
// Every number is a string, so the canonical JSON is exact.
type ReserveSnapshot struct {
	Version         string            `json:"version"`
	Disclaimer      string            `json:"disclaimer"`
	StableAssetID   string            `json:"stable_asset_id"`
	AssetID         string            `json:"asset_id"`
	Chain           string            `json:"chain"`
	Mint            string            `json:"mint"`
	Seq             string            `json:"seq"`
	TakenAt         time.Time         `json:"taken_at"`
	PrevHash        string            `json:"prev_hash"`
	LedgerSupply    string            `json:"ledger_supply"`
	ChainSupply     string            `json:"chain_supply"`
	PendingMint     string            `json:"pending_mint"`
	PendingBurn     string            `json:"pending_burn"`
	Peg             PriceDoc          `json:"peg"`
	Reserves        []SnapshotReserve `json:"reserves"`
	Composition     []CompositionLine `json:"composition"`
	SupplyValue     string            `json:"supply_value"`
	EligibleReserve string            `json:"eligible_reserve"`
	CoverageBPS     string            `json:"coverage_bps"`
}

// SignedSnapshot is a snapshot with its canonical body, hash and signature.
type SignedSnapshot struct {
	Snapshot    ReserveSnapshot
	Body        []byte
	Hash        [32]byte
	Signature   []byte
	PublicKey   ed25519.PublicKey
	EvidenceRef string
}

func snapshotSigningMessage(hash [32]byte) []byte {
	return append([]byte(snapshotSignatureDomain), hash[:]...)
}

// TakeSnapshot records the next reserve snapshot of an attestable stable
// asset in the caller's transaction: canonical JSON, hash-linked to the
// previous snapshot (the database refuses anything else, SV007), signed with
// the injected Ed25519 snapshot key and archived through econ.RecordEvidence.
func (s *Service) TakeSnapshot(ctx context.Context, tx pgx.Tx, sid StableAssetID) (SignedSnapshot, error) {
	a, err := s.get(ctx, tx, sid, true)
	if err != nil {
		return SignedSnapshot{}, err
	}
	if !a.Attestable {
		return SignedSnapshot{}, errs.New(errs.CodeValidationFailed, "stableasset: this stable asset does not publish reserve snapshots")
	}
	now := s.clk.Now().UTC().Truncate(time.Microsecond)
	rl, err := s.reserveLedger(ctx, tx, a, now)
	if err != nil {
		return SignedSnapshot{}, err
	}
	var supply uint64
	if err := s.host.ViewIn(ctx, tx, func(v devnethost.View) error {
		var err error
		supply, err = v.Supply(a.Chain, a.Mint)
		return err
	}); err != nil {
		return SignedSnapshot{}, err
	}
	var lastSeq int64
	var lastHash []byte
	err = tx.QueryRow(ctx, `SELECT seq, body_hash FROM stable_reserve_snapshots WHERE stable_asset_id = $1
		ORDER BY seq DESC LIMIT 1`, a.ID).Scan(&lastSeq, &lastHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return SignedSnapshot{}, dbErr("read last snapshot", err)
	}
	body := ReserveSnapshot{
		Version: snapshotVersion, Disclaimer: SnapshotDisclaimer, StableAssetID: a.ID.String(), AssetID: a.AssetID.String(),
		Chain: string(a.Chain), Mint: a.Mint.String(), Seq: strconv.FormatInt(lastSeq+1, 10), TakenAt: now,
		PrevHash: hex.EncodeToString(lastHash), LedgerSupply: rl.IssuedSupply.String(),
		ChainSupply: devnet.QuantityOf(supply).String(), PendingMint: rl.PendingMint.String(), PendingBurn: rl.PendingBurn.String(),
		Peg: priceDoc(a.Peg), Reserves: snapshotReserves(rl), Composition: composition(rl),
		SupplyValue: rl.SupplyValue.String(), EligibleReserve: rl.EligibleReserve.String(),
		CoverageBPS: strconv.FormatInt(int64(rl.CoverageBPS), 10),
	}
	canonical, err := audit.CanonicalJSON(body)
	if err != nil {
		return SignedSnapshot{}, errs.Wrap(err, errs.CodeInternal, "stableasset: snapshot is not canonically encodable")
	}
	hash := sha256.Sum256(canonical)
	sig := s.snapshots.sign(snapshotSigningMessage(hash))
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key:      "stableasset/snapshots/" + a.ID.String() + "/" + body.Seq,
		Document: map[string]any{"snapshot": json.RawMessage(canonical), "hash": hex.EncodeToString(hash[:]), "signature": hex.EncodeToString(sig), "public_key": hex.EncodeToString(s.snapshots.public)},
	})
	if err != nil {
		return SignedSnapshot{}, err
	}
	var prev any
	if len(lastHash) > 0 {
		prev = lastHash
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stable_reserve_snapshots
		(id, stable_asset_id, seq, body, body_hash, prev_hash, signature, signer_public_key, evidence_ref, taken_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id.New[id.Any](), a.ID, lastSeq+1, string(canonical), hash[:], prev, sig,
		[]byte(s.snapshots.public), ev.URI, now); err != nil {
		return SignedSnapshot{}, dbErr("record snapshot", err)
	}
	return SignedSnapshot{Snapshot: body, Body: canonical, Hash: hash, Signature: sig, PublicKey: s.snapshots.PublicKey(), EvidenceRef: ev.URI}, nil
}

func snapshotReserves(rl ReserveLedger) []SnapshotReserve {
	var out []SnapshotReserve
	for _, h := range append(append([]ReserveHolding(nil), rl.CashEquivalentReserve...), rl.TokenizedReserve...) {
		out = append(out, SnapshotReserve{
			AssetID: h.AssetID.String(), Kind: string(h.Kind), Balance: h.Quantity.String(),
			Decimals: strconv.Itoa(int(h.Decimals)), FactorBPS: strconv.FormatInt(int64(h.FactorBPS), 10),
			Price: h.Price, Eligible: h.Eligible.String(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID < out[j].AssetID })
	if out == nil {
		out = []SnapshotReserve{}
	}
	return out
}

// composition is the collateral composition: each reserve kind's share of the
// eligible reserve, in basis points (rounded down).
func composition(rl ReserveLedger) []CompositionLine {
	total := rl.EligibleReserve
	out := []CompositionLine{}
	for _, part := range []struct {
		kind ReserveKind
		hs   []ReserveHolding
	}{{ReserveCashEquivalent, rl.CashEquivalentReserve}, {ReserveTokenized, rl.TokenizedReserve}} {
		e := sumEligible(part.hs)
		share := money.BPS(0)
		if total.Coef.Sign() > 0 {
			share = ratioBPS(e, total)
		}
		out = append(out, CompositionLine{Kind: string(part.kind), Eligible: e.String(), ShareBPS: strconv.FormatInt(int64(share), 10)})
	}
	return out
}

// Snapshots returns a stable asset's snapshots in order.
func (s *Service) Snapshots(ctx context.Context, q db.Querier, sid StableAssetID) ([]SignedSnapshot, error) {
	rows, err := q.Query(ctx, `SELECT body, body_hash, signature, signer_public_key, evidence_ref
		FROM stable_reserve_snapshots WHERE stable_asset_id = $1 ORDER BY seq`, sid)
	if err != nil {
		return nil, dbErr("list snapshots", err)
	}
	defer rows.Close()
	var out []SignedSnapshot
	for rows.Next() {
		var body string
		var hash, sig, key []byte
		var ref string
		if err := rows.Scan(&body, &hash, &sig, &key, &ref); err != nil {
			return nil, dbErr("scan snapshot", err)
		}
		snap, err := ParseSignedSnapshot([]byte(body), hash, sig, key)
		if err != nil {
			return nil, err
		}
		snap.EvidenceRef = ref
		out = append(out, snap)
	}
	return out, rows.Err()
}

// ParseSignedSnapshot decodes a stored snapshot strictly. It never panics.
func ParseSignedSnapshot(body, hash, sig, key []byte) (SignedSnapshot, error) {
	if len(hash) != 32 || len(sig) != ed25519.SignatureSize || len(key) != ed25519.PublicKeySize {
		return SignedSnapshot{}, errs.New(errs.CodeValidationFailed, "stableasset: snapshot hash, signature or key has the wrong length")
	}
	var snap ReserveSnapshot
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return SignedSnapshot{}, errs.New(errs.CodeValidationFailed, "stableasset: snapshot body does not decode")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SignedSnapshot{}, errs.New(errs.CodeValidationFailed, "stableasset: trailing data after the snapshot body")
	}
	out := SignedSnapshot{Snapshot: snap, Body: append([]byte(nil), body...), Signature: append([]byte(nil), sig...),
		PublicKey: append(ed25519.PublicKey(nil), key...)}
	copy(out.Hash[:], hash)
	return out, nil
}

// Snapshot verification failure reasons.
const (
	SnapshotBodyNotCanonical = "SNAPSHOT_BODY_NOT_CANONICAL"
	SnapshotHashMismatch     = "SNAPSHOT_HASH_MISMATCH"
	SnapshotSignatureInvalid = "SNAPSHOT_SIGNATURE_INVALID"
	SnapshotChainBroken      = "SNAPSHOT_CHAIN_BROKEN"
	SnapshotKeyMismatch      = "SNAPSHOT_KEY_MISMATCH"
	SnapshotNotADisclaimer   = "SNAPSHOT_DISCLAIMER_MISSING"
)

func snapshotInvalid(seq int, reason string) error {
	return errs.New(errs.CodeValidationFailed, "stableasset: reserve snapshot chain does not verify").
		WithField("index", seq).WithField("reason", reason)
}

// VerifySnapshotChain verifies a stable asset's snapshots end to end, from
// nothing but the snapshots and the verification key: each body re-encodes
// to exactly its stored canonical bytes, hashes to its recorded hash, carries
// a valid Ed25519 signature by key over that hash, names the previous
// snapshot's hash (the first names none), counts up from 1, belongs to the
// same stable asset, and carries the disclaimer.
func VerifySnapshotChain(snaps []SignedSnapshot, key ed25519.PublicKey) error {
	if len(key) != ed25519.PublicKeySize {
		return errs.New(errs.CodeValidationFailed, "stableasset: snapshot verification key has the wrong length")
	}
	var prev [32]byte
	for i, sn := range snaps {
		canonical, err := audit.CanonicalJSON(sn.Snapshot)
		if err != nil || !bytes.Equal(canonical, sn.Body) {
			return snapshotInvalid(i, SnapshotBodyNotCanonical)
		}
		if sha256.Sum256(sn.Body) != sn.Hash {
			return snapshotInvalid(i, SnapshotHashMismatch)
		}
		if !bytes.Equal(sn.PublicKey, key) {
			return snapshotInvalid(i, SnapshotKeyMismatch)
		}
		if !ed25519.Verify(key, snapshotSigningMessage(sn.Hash), sn.Signature) {
			return snapshotInvalid(i, SnapshotSignatureInvalid)
		}
		if sn.Snapshot.Disclaimer != SnapshotDisclaimer || sn.Snapshot.Version != snapshotVersion {
			return snapshotInvalid(i, SnapshotNotADisclaimer)
		}
		wantPrev := ""
		if i > 0 {
			wantPrev = hex.EncodeToString(prev[:])
		}
		if sn.Snapshot.Seq != strconv.Itoa(i+1) || sn.Snapshot.PrevHash != wantPrev ||
			(i > 0 && sn.Snapshot.StableAssetID != snaps[0].Snapshot.StableAssetID) {
			return snapshotInvalid(i, SnapshotChainBroken)
		}
		prev = sn.Hash
	}
	return nil
}
