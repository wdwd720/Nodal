package marketplace

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
)

// Provenance is everything a version can be traced to: where its source came
// from, what built it, what it was evaluated on, and what it was forked from.
type Provenance struct {
	VersionID    VersionID
	StrategyRef  string
	SourceCommit string
	SourceDigest []byte
	ManifestHash []byte
	// From the validation; empty until the version has been built.
	ArtifactDigest   []byte
	ToolchainVersion string
	SDKVersion       string
	RuntimeVersion   string
	// From the latest evaluation; empty until the version has been simulated.
	DatasetVersion string
	EvaluationID   *EvaluationID
	// The fork chain, nearest parent first: every version this listing
	// descends from.
	ForkedFrom []ForkAncestor
}

// ForkAncestor is one link of a fork chain.
type ForkAncestor struct {
	VersionID    VersionID
	ListingID    ListingID
	Number       int
	SourceDigest []byte
	ManifestHash []byte
}

// maxForkDepth bounds the walk up a fork chain.
const maxForkDepth = 64

// Provenance assembles a version's provenance.
func (s *Service) Provenance(ctx context.Context, q db.Querier, vid VersionID) (Provenance, error) {
	v, err := s.Version(ctx, q, vid)
	if err != nil {
		return Provenance{}, err
	}
	p := Provenance{VersionID: v.ID, StrategyRef: v.StrategyRef(), SourceCommit: v.SourceCommit, SourceDigest: v.SourceDigest,
		ManifestHash: v.ManifestHash}
	if val, ok, err := s.ValidationOf(ctx, q, vid); err != nil {
		return Provenance{}, err
	} else if ok {
		p.ArtifactDigest, p.ToolchainVersion, p.SDKVersion, p.RuntimeVersion = val.ArtifactDigest, val.ToolchainVersion, val.SDKVersion, val.RuntimeVersion
	}
	var evalID EvaluationID
	err = q.QueryRow(ctx, `SELECT id, dataset_version FROM marketplace_evaluations WHERE version_id = $1
		ORDER BY created_at DESC, id DESC LIMIT 1`, vid).Scan(&evalID, &p.DatasetVersion)
	switch {
	case err == nil:
		p.EvaluationID = &evalID
	case !errors.Is(err, pgx.ErrNoRows):
		return Provenance{}, dbErr("load evaluation", err)
	}
	listing, err := s.Listing(ctx, q, v.ListingID)
	if err != nil {
		return Provenance{}, err
	}
	next := listing.ForkedFromVersionID
	for depth := 0; next != nil && depth < maxForkDepth; depth++ {
		parent, err := s.Version(ctx, q, *next)
		if err != nil {
			return Provenance{}, err
		}
		p.ForkedFrom = append(p.ForkedFrom, ForkAncestor{VersionID: parent.ID, ListingID: parent.ListingID, Number: parent.Number,
			SourceDigest: parent.SourceDigest, ManifestHash: parent.ManifestHash})
		pl, err := s.Listing(ctx, q, parent.ListingID)
		if err != nil {
			return Provenance{}, err
		}
		next = pl.ForkedFromVersionID
	}
	return p, nil
}
