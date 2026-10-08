package marketplace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/sandboxrt"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
	"github.com/nodal/controlplane/internal/security"
)

// Content bounds of a version (the database refuses beyond them too).
const (
	maxSourceFiles       = 32
	maxSourceFileBytes   = 64 << 10
	maxDataDependencies  = 64
	maxSupported         = 256
	maxPerformanceFeeBPS = 5000
	maxManagementFeeBPS  = 500
)

var (
	sourceNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}\.go$`)
	reasonCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
)

// VersionRequest is the content of a version.
type VersionRequest struct {
	Description            string
	RiskClass              RiskClass
	SupportedInstrumentIDs []string
	Manifest               sandboxrt.Manifest
	Source                 sandboxrt.Source
	// SourceCommit names the commit the source was taken from.
	SourceCommit        string
	BacktestEvidenceRef string
	DataDependencies    []string
	Fees                FeeModel
	// Config is the configuration a run sees, string to string.
	Config map[string]string
}

func (r VersionRequest) validate() error {
	var problems []string
	if d := strings.TrimSpace(r.Description); d == "" || len(r.Description) > 4000 || !utf8.ValidString(r.Description) {
		problems = append(problems, "description must be 1..4000 characters")
	}
	if !r.RiskClass.Valid() {
		problems = append(problems, fmt.Sprintf("unknown risk class %q", r.RiskClass))
	}
	if err := r.Manifest.Validate(); err != nil {
		problems = append(problems, "manifest: "+err.Error())
	}
	if len(r.SupportedInstrumentIDs) > maxSupported {
		problems = append(problems, "too many supported instruments")
	}
	for _, s := range r.SupportedInstrumentIDs {
		if !nodalsdk.ValidID(s) {
			problems = append(problems, fmt.Sprintf("supported instrument %q is not a canonical uuid", s))
		}
	}
	if len(r.Source) == 0 || len(r.Source) > maxSourceFiles {
		problems = append(problems, fmt.Sprintf("a version has 1..%d source files", maxSourceFiles))
	}
	for name, body := range r.Source {
		if !sourceNamePattern.MatchString(name) || strings.HasSuffix(name, "_test.go") || len(body) > maxSourceFileBytes {
			problems = append(problems, fmt.Sprintf("source file %q must be [a-z0-9_]{1,64}.go of at most %d bytes", name, maxSourceFileBytes))
		}
	}
	if c := strings.TrimSpace(r.SourceCommit); c == "" || c != r.SourceCommit || len(c) > 100 {
		problems = append(problems, "source commit must be 1..100 characters")
	}
	if len(r.BacktestEvidenceRef) > 500 {
		problems = append(problems, "backtest evidence reference too long")
	}
	if len(r.DataDependencies) > maxDataDependencies {
		problems = append(problems, "too many data dependencies")
	}
	for _, d := range r.DataDependencies {
		if !nodalsdk.ValidName(d) {
			problems = append(problems, fmt.Sprintf("data dependency %q must be a lower-case name", d))
		}
	}
	if r.Fees.PerformanceFeeBPS < 0 || r.Fees.PerformanceFeeBPS > maxPerformanceFeeBPS ||
		r.Fees.ManagementFeeBPS < 0 || r.Fees.ManagementFeeBPS > maxManagementFeeBPS {
		problems = append(problems, "fee metadata out of range")
	}
	if len(r.Config) > nodalsdk.MaxConfigEntries {
		problems = append(problems, "too many configuration entries")
	}
	for k, v := range r.Config {
		if !nodalsdk.ValidName(k) || len(v) > nodalsdk.MaxConfigValueBytes || !utf8.ValidString(v) {
			problems = append(problems, fmt.Sprintf("configuration entry %q is malformed", k))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errs.New(errs.CodeValidationFailed, "marketplace: invalid listing version").WithField("problems", problems)
	}
	return nil
}

// ListingRequest creates a listing and its first version.
type ListingRequest struct {
	CreatorAccountID accounts.AccountID
	Creator          econ.Actor
	Title            string
	Version          VersionRequest
}

// CreateListing creates a listing with version 1 in DRAFT.
func (s *Service) CreateListing(ctx context.Context, tx pgx.Tx, req ListingRequest) (Listing, Version, error) {
	return s.createListing(ctx, tx, req, nil)
}

func (s *Service) createListing(ctx context.Context, tx pgx.Tx, req ListingRequest, parent *Version) (Listing, Version, error) {
	if err := requireActor(req.Creator, CreatorActorTypes(), "publish a strategy"); err != nil {
		return Listing{}, Version{}, err
	}
	if req.CreatorAccountID.IsZero() {
		return Listing{}, Version{}, errs.New(errs.CodeValidationFailed, "marketplace: a creator account is required")
	}
	if t := strings.TrimSpace(req.Title); t == "" || len(req.Title) > 200 || !utf8.ValidString(req.Title) {
		return Listing{}, Version{}, errs.New(errs.CodeValidationFailed, "marketplace: title must be 1..200 characters")
	}
	if err := req.Version.validate(); err != nil {
		return Listing{}, Version{}, err
	}
	if err := s.authorize(ctx, tx, gates.ModeSandbox); err != nil {
		return Listing{}, Version{}, err
	}
	lid := id.New[listingKind]()
	var forked any
	if parent != nil {
		forked = parent.ID
	}
	l, err := scanListing(tx.QueryRow(ctx, `INSERT INTO marketplace_listings (id, creator_account_id, creator_actor_type, creator_actor_id, title, forked_from_version_id)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+listingColumns, lid, req.CreatorAccountID, req.Creator.Type, req.Creator.ID, req.Title, forked))
	if err != nil {
		return Listing{}, Version{}, dbErr("create listing", err)
	}
	v, err := s.insertVersion(ctx, tx, l, 1, req.Creator, req.Version)
	if err != nil {
		return Listing{}, Version{}, err
	}
	payload := map[string]string{"listing_id": l.ID.String(), "version_id": v.ID.String(), "title": l.Title}
	action := "marketplace.listing.created"
	if parent != nil {
		payload["forked_from_version_id"] = parent.ID.String()
		action = "marketplace.listing.forked"
	}
	if err := s.auditEvent(ctx, tx, audit.AccountStream(req.CreatorAccountID.String()), req.Creator, action, "marketplace_listing",
		l.ID.String(), action, payload); err != nil {
		return Listing{}, Version{}, err
	}
	return l, v, nil
}

func (s *Service) insertVersion(ctx context.Context, tx pgx.Tx, l Listing, number int, by econ.Actor, req VersionRequest) (Version, error) {
	canonical, err := req.Manifest.CanonicalBytes()
	if err != nil {
		return Version{}, err
	}
	hash, err := req.Manifest.Hash()
	if err != nil {
		return Version{}, err
	}
	config := req.Config
	if config == nil {
		config = map[string]string{}
	}
	configJSON, err := audit.CanonicalJSON(config)
	if err != nil {
		return Version{}, errs.Wrap(err, errs.CodeValidationFailed, "marketplace: configuration is not encodable")
	}
	supported := sortedCopy(req.SupportedInstrumentIDs)
	deps := sortedCopy(req.DataDependencies)
	vid := id.New[versionKind]()
	v, err := scanVersion(tx.QueryRow(ctx, `INSERT INTO marketplace_listing_versions (id, listing_id, version, status, moderation_state,
		description, risk_class, supported_instrument_ids, manifest_canonical, manifest_hash, source_digest, source_commit,
		backtest_evidence_ref, data_dependencies, performance_fee_bps, management_fee_bps, fee_high_water_mark, config,
		created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,'DRAFT','UNREVIEWED',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) RETURNING `+versionColumns,
		vid, l.ID, number, req.Description, req.RiskClass, supported, canonical, hash, req.Source.Digest(), req.SourceCommit,
		req.BacktestEvidenceRef, deps, int64(req.Fees.PerformanceFeeBPS), int64(req.Fees.ManagementFeeBPS), req.Fees.HighWaterMark,
		configJSON, by.Type, by.ID))
	if err != nil {
		return Version{}, dbErr("create version", err)
	}
	for _, name := range req.Source.Paths() {
		if _, err := tx.Exec(ctx, `INSERT INTO marketplace_version_sources (id, version_id, path, content) VALUES ($1,$2,$3,$4)`,
			id.New[id.Any](), v.ID, name, req.Source[name]); err != nil {
			return Version{}, dbErr("store source", err)
		}
	}
	return v, nil
}

// NewVersion publishes the next version of a listing, in DRAFT. Only the
// listing's creator may; earlier versions are untouched.
func (s *Service) NewVersion(ctx context.Context, tx pgx.Tx, lid ListingID, by econ.Actor, req VersionRequest) (Version, error) {
	if err := requireActor(by, CreatorActorTypes(), "publish a strategy"); err != nil {
		return Version{}, err
	}
	if err := req.validate(); err != nil {
		return Version{}, err
	}
	l, err := s.Listing(ctx, tx, lid)
	if err != nil {
		return Version{}, err
	}
	if by.ID != l.Creator.ID {
		return Version{}, errs.New(errs.CodeForbidden, "marketplace: only the listing's creator publishes its versions")
	}
	if err := s.authorize(ctx, tx, gates.ModeSandbox); err != nil {
		return Version{}, err
	}
	var next int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) + 1 FROM marketplace_listing_versions WHERE listing_id = $1`, lid).Scan(&next); err != nil {
		return Version{}, dbErr("next version", err)
	}
	// Two concurrent publishers compute the same number; the UNIQUE
	// (listing_id, version) makes the second a CONFLICT, never a duplicate.
	v, err := s.insertVersion(ctx, tx, l, next, by, req)
	if err != nil {
		return Version{}, err
	}
	if err := s.auditEvent(ctx, tx, audit.AccountStream(l.CreatorAccountID.String()), by, "marketplace.version.published",
		"marketplace_listing_version", v.ID.String(), "new version", map[string]any{"listing_id": lid.String(), "version": v.Number}); err != nil {
		return Version{}, err
	}
	return v, nil
}

// ForkRequest forks a public version into a new listing.
type ForkRequest struct {
	CreatorAccountID accounts.AccountID
	Creator          econ.Actor
	Title            string
	// Version is the fork's content. A nil Source or a zero Manifest takes
	// the parent's.
	Version VersionRequest
}

// Fork creates a new listing whose provenance points at an APPROVED or
// DEPRECATED parent version. The fork starts at DRAFT and goes through the
// whole lifecycle again; it inherits nothing but its provenance (and, if the
// request leaves them out, the parent's source and manifest as a starting
// point).
func (s *Service) Fork(ctx context.Context, tx pgx.Tx, parentID VersionID, req ForkRequest) (Listing, Version, error) {
	parent, err := s.Version(ctx, tx, parentID)
	if err != nil {
		return Listing{}, Version{}, err
	}
	if parent.Status != StatusApproved && parent.Status != StatusDeprecated {
		return Listing{}, Version{}, errs.Newf(errs.CodeForbidden, "marketplace: a %s version cannot be forked", parent.Status)
	}
	vr := req.Version
	if vr.Source == nil {
		if vr.Source, err = s.Source(ctx, tx, parentID); err != nil {
			return Listing{}, Version{}, err
		}
	}
	if vr.Manifest.Schema == "" && len(vr.Manifest.AllowedIntentKinds) == 0 {
		vr.Manifest = parent.Manifest
	}
	return s.createListing(ctx, tx, ListingRequest{CreatorAccountID: req.CreatorAccountID, Creator: req.Creator, Title: req.Title, Version: vr}, &parent)
}

// Submit publishes a DRAFT for validation. Only its author may.
func (s *Service) Submit(ctx context.Context, tx pgx.Tx, vid VersionID, by econ.Actor) (Version, error) {
	if err := requireActor(by, CreatorActorTypes(), "submit a strategy"); err != nil {
		return Version{}, err
	}
	v, err := s.loadVersion(ctx, tx, vid, true)
	if err != nil {
		return Version{}, err
	}
	if by.ID != v.CreatedBy.ID {
		return Version{}, errs.New(errs.CodeForbidden, "marketplace: only the version's author submits it")
	}
	if err := s.authorize(ctx, tx, gates.ModeSandbox); err != nil {
		return Version{}, err
	}
	return s.transition(ctx, tx, v, StatusSubmitted, by, "submitted for validation", "")
}

// Validation is a marketplace_version_validations row.
type Validation struct {
	VersionID        VersionID
	Outcome          ValidationOutcome
	ArtifactDigest   []byte
	FailureReason    string
	ScanCodes        []string
	ToolchainVersion string
	SDKVersion       string
	RuntimeVersion   string
}

// Validate scans and builds a SUBMITTED version. A clean scan and a
// successful build record the artifact and move the version to VALIDATED; a
// refused scan, a compile error or a build timeout moves it to REJECTED with
// the reason as its rejection code. Any other failure changes nothing.
//
// The build runs inside the caller's transaction, holding the version's row
// lock for its duration (seconds, bounded by the builder's timeout): run
// validation from a worker, not from a request path.
func (s *Service) Validate(ctx context.Context, tx pgx.Tx, vid VersionID, by econ.Actor) (Version, Validation, error) {
	if err := requireActor(by, TransitionActorTypes(), "validate a strategy"); err != nil {
		return Version{}, Validation{}, err
	}
	v, err := s.loadVersion(ctx, tx, vid, true)
	if err != nil {
		return Version{}, Validation{}, err
	}
	if v.Status != StatusSubmitted {
		return Version{}, Validation{}, errs.Newf(errs.CodeInvalidStateTransition, "marketplace: a %s version is not awaiting validation", v.Status)
	}
	if err := s.authorize(ctx, tx, gates.ModeSandbox); err != nil {
		return Version{}, Validation{}, err
	}
	src, err := s.Source(ctx, tx, vid)
	if err != nil {
		return Version{}, Validation{}, err
	}
	if !bytes.Equal(src.Digest(), v.SourceDigest) {
		return Version{}, Validation{}, errs.New(errs.CodeInternal, "marketplace: stored source does not match its digest")
	}
	build, buildErr := s.cfg.Builder.Build(ctx, src)
	val := Validation{VersionID: vid, ScanCodes: build.Scan.Codes(), ToolchainVersion: build.ToolchainVersion,
		SDKVersion: build.SDKVersion, RuntimeVersion: s.cfg.RuntimeVersion}
	if buildErr != nil {
		if errs.CodeOf(buildErr) != errs.CodeStrategyRejected || build.Failure == "" {
			return Version{}, Validation{}, buildErr
		}
		val.Outcome, val.FailureReason = ValidationFailed, build.Failure
	} else {
		if !bytes.Equal(build.SourceDigest, v.SourceDigest) {
			return Version{}, Validation{}, errs.New(errs.CodeInternal, "marketplace: the build is not of this version's source")
		}
		if _, _, err := s.cfg.Sandbox.RecordArtifact(ctx, tx, build, by); err != nil {
			return Version{}, Validation{}, err
		}
		val.Outcome, val.ArtifactDigest = ValidationPassed, build.ArtifactDigest
	}
	var failure any
	if val.FailureReason != "" {
		failure = val.FailureReason
	}
	var logDigest any
	if len(build.BuildLogDigest) == 32 {
		logDigest = build.BuildLogDigest
	}
	if _, err := tx.Exec(ctx, `INSERT INTO marketplace_version_validations (id, version_id, outcome, artifact_digest, failure_reason, scan_codes,
		build_log_digest, toolchain_version, sdk_version, runtime_version, validated_by_actor_type, validated_by_actor_id, validated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id.New[id.Any](), vid, val.Outcome, val.ArtifactDigest, failure, val.ScanCodes, logDigest, val.ToolchainVersion,
		val.SDKVersion, val.RuntimeVersion, by.Type, by.ID, s.now()); err != nil {
		return Version{}, Validation{}, dbErr("record validation", err)
	}
	if val.Outcome == ValidationFailed {
		out, err := s.transition(ctx, tx, v, StatusRejected, by, "validation failed: "+strings.Join(val.ScanCodes, ","), val.FailureReason)
		return out, val, err
	}
	out, err := s.transition(ctx, tx, v, StatusValidated, by, "scan and build passed", "")
	return out, val, err
}

// ValidationOf returns a version's validation, if it has one.
func (s *Service) ValidationOf(ctx context.Context, q db.Querier, vid VersionID) (Validation, bool, error) {
	val := Validation{VersionID: vid}
	var failure *string
	err := q.QueryRow(ctx, `SELECT outcome, artifact_digest, failure_reason, scan_codes, toolchain_version, sdk_version, runtime_version
		FROM marketplace_version_validations WHERE version_id = $1`, vid).Scan(&val.Outcome, &val.ArtifactDigest, &failure, &val.ScanCodes,
		&val.ToolchainVersion, &val.SDKVersion, &val.RuntimeVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Validation{}, false, nil
		}
		return Validation{}, false, dbErr("load validation", err)
	}
	if failure != nil {
		val.FailureReason = *failure
	}
	return val, true, nil
}

// requireReviewer refuses anyone but an OPERATOR who neither created the
// listing nor authored the version: dual control. The database enforces the
// same rule on every review row (MK002).
func (s *Service) requireReviewer(ctx context.Context, q pgx.Tx, v Version, reviewer econ.Actor) error {
	if err := requireActor(reviewer, ReviewerActorTypes(), "review a strategy"); err != nil {
		return err
	}
	l, err := s.Listing(ctx, q, v.ListingID)
	if err != nil {
		return err
	}
	if reviewer.ID == l.Creator.ID || reviewer.ID == v.CreatedBy.ID {
		return errs.New(errs.CodeForbidden, "marketplace: the creator of a strategy cannot review it").
			WithField("version_id", v.ID.String())
	}
	return nil
}

// StartReview takes a VALIDATED version into review.
func (s *Service) StartReview(ctx context.Context, tx pgx.Tx, vid VersionID, reviewer econ.Actor) (Version, error) {
	v, err := s.loadVersion(ctx, tx, vid, true)
	if err != nil {
		return Version{}, err
	}
	if err := s.requireReviewer(ctx, tx, v, reviewer); err != nil {
		return Version{}, err
	}
	return s.transition(ctx, tx, v, StatusInReview, reviewer, "review started", "")
}

func (s *Service) recordReview(ctx context.Context, tx pgx.Tx, v Version, reviewer econ.Actor, d ReviewDecision, reasonCode, note string) error {
	if !reasonCodePattern.MatchString(reasonCode) {
		return errs.New(errs.CodeValidationFailed, "marketplace: a review needs a reason code [A-Z][A-Z0-9_]{1,63}")
	}
	if len(note) > 2000 || !utf8.ValidString(note) {
		return errs.New(errs.CodeValidationFailed, "marketplace: review note too long")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO marketplace_reviews (id, version_id, reviewer_actor_type, reviewer_actor_id, decision, reason_code, note, reviewed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id.New[id.Any](), v.ID, reviewer.Type, reviewer.ID, d, reasonCode, note, s.now()); err != nil {
		return dbErr("record review", err)
	}
	return nil
}

// Approve approves a version IN_REVIEW and clears it for installs. The
// reviewer is an OPERATOR other than the creator.
func (s *Service) Approve(ctx context.Context, tx pgx.Tx, vid VersionID, reviewer econ.Actor, reasonCode, note string) (Version, error) {
	v, err := s.loadVersion(ctx, tx, vid, true)
	if err != nil {
		return Version{}, err
	}
	if err := s.requireReviewer(ctx, tx, v, reviewer); err != nil {
		return Version{}, err
	}
	if v.Status != StatusInReview {
		return Version{}, errs.Newf(errs.CodeInvalidStateTransition, "marketplace: a %s version cannot be approved", v.Status)
	}
	if err := s.authorize(ctx, tx, gates.ModeSandbox); err != nil {
		return Version{}, err
	}
	if err := s.recordReview(ctx, tx, v, reviewer, DecisionApprove, reasonCode, note); err != nil {
		return Version{}, err
	}
	out, err := s.transition(ctx, tx, v, StatusApproved, reviewer, "approved: "+reasonCode, "")
	if err != nil {
		return Version{}, err
	}
	if out.Moderation == ModerationUnreviewed {
		return s.moderate(ctx, tx, out, ModerationCleared, reviewer, "cleared on approval")
	}
	return out, nil
}

// Reject rejects a version that is SUBMITTED, VALIDATED or IN_REVIEW.
func (s *Service) Reject(ctx context.Context, tx pgx.Tx, vid VersionID, reviewer econ.Actor, reasonCode, note string) (Version, error) {
	v, err := s.loadVersion(ctx, tx, vid, true)
	if err != nil {
		return Version{}, err
	}
	if err := s.requireReviewer(ctx, tx, v, reviewer); err != nil {
		return Version{}, err
	}
	if !CanTransition(v.Status, StatusRejected) {
		return Version{}, errs.Newf(errs.CodeInvalidStateTransition, "marketplace: a %s version cannot be rejected", v.Status)
	}
	if err := s.recordReview(ctx, tx, v, reviewer, DecisionReject, reasonCode, note); err != nil {
		return Version{}, err
	}
	return s.transition(ctx, tx, v, StatusRejected, reviewer, "rejected: "+reasonCode, reasonCode)
}

// Deprecate retires an APPROVED version: no new installs; existing installs
// can no longer be followed. Its creator or an operator may.
func (s *Service) Deprecate(ctx context.Context, tx pgx.Tx, vid VersionID, by econ.Actor, reason string) (Version, error) {
	if err := requireActor(by, CreatorActorTypes(), "deprecate a strategy"); err != nil {
		return Version{}, err
	}
	v, err := s.loadVersion(ctx, tx, vid, true)
	if err != nil {
		return Version{}, err
	}
	if by.Type != security.ActorOperator && by.ID != v.CreatedBy.ID {
		return Version{}, errs.New(errs.CodeForbidden, "marketplace: only the author or an operator deprecates a version")
	}
	return s.transition(ctx, tx, v, StatusDeprecated, by, reason, "")
}

// Moderate flags, takes down or re-clears a version. Operators only. A
// flagged or taken-down version cannot be installed or followed.
func (s *Service) Moderate(ctx context.Context, tx pgx.Tx, vid VersionID, operator econ.Actor, to Moderation, reason string) (Version, error) {
	if err := requireActor(operator, ReviewerActorTypes(), "moderate a strategy"); err != nil {
		return Version{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Version{}, errs.New(errs.CodeValidationFailed, "marketplace: moderation needs a reason")
	}
	v, err := s.loadVersion(ctx, tx, vid, true)
	if err != nil {
		return Version{}, err
	}
	return s.moderate(ctx, tx, v, to, operator, reason)
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
