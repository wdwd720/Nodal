package econ

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/proof"
)

// EvidenceRef points at an archived evidence document.
type EvidenceRef struct {
	URI    string
	SHA256 []byte
}

// Evidence is a canonical evidence document for one economic effect: what
// was decided, against what inputs, producing what result. It is written
// create-only into the evidence archive inside the effect's own transaction,
// so the effect and its evidence commit together or not at all.
type Evidence struct {
	// Key names the document; it must be unique, e.g. "p2p/<transfer id>".
	Key string
	// Document is any canonically encodable value (money as strings).
	Document any
}

// RecordEvidence archives the canonical JSON of e.Document under
// "advanced/<key>.json" and returns its URI and digest. Writing the same key
// twice with identical bytes is not an error (a replay); different bytes are.
func RecordEvidence(ctx context.Context, tx pgx.Tx, e Evidence) (EvidenceRef, error) {
	key := strings.TrimSpace(e.Key)
	if key == "" || key != e.Key || strings.Contains(key, "..") {
		return EvidenceRef{}, errs.New(errs.CodeValidationFailed, "econ: evidence key must be non-empty, trimmed and contain no '..'")
	}
	body, err := audit.CanonicalJSON(e.Document)
	if err != nil {
		return EvidenceRef{}, errs.Wrap(err, errs.CodeValidationFailed, "econ: evidence is not canonically encodable")
	}
	archive, err := proof.NewPgArchive(tx)
	if err != nil {
		return EvidenceRef{}, errs.Wrap(err, errs.CodeInternal, "econ: open evidence archive")
	}
	uri, sum, err := archive.Put(ctx, "advanced/"+key+".json", body, nil)
	if err != nil {
		if errors.Is(err, proof.ErrObjectExistsIdentical) {
			digest := sha256.Sum256(body)
			return EvidenceRef{URI: "pg://provider_evidence/advanced/" + key + ".json", SHA256: digest[:]}, nil
		}
		return EvidenceRef{}, errs.Wrap(err, errs.CodeConflict, "econ: archive evidence")
	}
	return EvidenceRef{URI: uri, SHA256: sum}, nil
}
