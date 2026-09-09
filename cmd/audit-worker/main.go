// Command audit-worker builds signed Merkle checkpoints over the audit hash
// chain, archives them, verifies the whole history, and prints PART 88
// proof bundles.
//
// Usage:
//
//	audit-worker run              periodic checkpoints (CP_AUDIT_CHECKPOINT_INTERVAL, default 5m) and
//	                              full verification (CP_AUDIT_VERIFY_INTERVAL, default 1h) until SIGINT/SIGTERM
//	audit-worker checkpoint       create one checkpoint over every uncovered event (full sweep) and exit
//	audit-worker verify           verify chains, checkpoint signatures, roots and archived objects;
//	                              prints the report as JSON; exit 1 on any failure (make verify-audit)
//	audit-worker bundle <id>      print the proof bundle for a fill id or a trade intent id
//	audit-worker purge            one retention pass over login_attempts and exit
//
// Retention runs here because this binary is already the one that owns what
// the platform keeps and for how long: it reads CP_RETENTION_SECURITY_AUDIT_DAYS
// for the archive's Object Lock window. The purge needs the cp_ops role
// (CP_DATABASE_OPS_URL), because cp_app is deliberately refused DELETE on the
// rows it removes, and this binary refuses to run it rather than skipping when
// that URL is unset (F-79).
//
// Configuration comes from internal/config (CP_* variables). The signer is
// KMS (CP_KMS_AUDIT_SIGNING_KEY_ID, CP_KMS_REGION) unless the environment is
// LOCAL, TEST or DEV and CP_AUDIT_LOCAL_SIGNING_KEY_REF names a SecretRef to
// a PEM P-256 private key. The archive is the Object Lock bucket wired by the
// composition root; until then CP_AUDIT_ARCHIVE_DIR (LOCAL/TEST/DEV only)
// selects a filesystem archive. verify runs without keys or archive only
// while the database holds no checkpoint. Key material is never logged.
//
// Key rotation (D-029): checkpoints outlive keys, so verification uses the
// key each row names, taken from a trusted set rather than from "the key
// this process holds". The active signing key is trusted automatically;
// rotated-out keys must stay trusted or their checkpoints stop verifying:
//
//	CP_AUDIT_RETIRED_SIGNING_KEY_IDS   comma-separated KMS key ARNs, rotated out but still trusted
//	CP_AUDIT_LOCAL_RETIRED_KEY_REFS    comma-separated SecretRefs to retired PEM keys (LOCAL/TEST/DEV)
//	CP_AUDIT_REVOKED_SIGNING_KEY_IDS   comma-separated key ids whose signatures are no longer accepted;
//	                                   this overrides active/retired and needs no key material
//
// Trust the ARN KMS reports, not an alias: an alias can be re-pointed, and
// the row records the ARN. A checkpoint naming an untrusted or revoked key
// fails as checkpoint_key_unknown / checkpoint_key_revoked, never as a bad
// signature, so a rotation gap is never read as tampering.
//
// Exit codes: 0 success (verify: history verified), 1 failure (verify:
// tampering or runtime error), 2 usage error.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/proof"
)

// Worker-specific variables (everything else is internal/config).
const (
	envCheckpointInterval = "CP_AUDIT_CHECKPOINT_INTERVAL"
	envVerifyInterval     = "CP_AUDIT_VERIFY_INTERVAL"
	envSweepEvery         = "CP_AUDIT_SWEEP_EVERY"
	envLocalKeyRef        = "CP_AUDIT_LOCAL_SIGNING_KEY_REF"  //nolint:gosec // G101: the name of a SecretRef variable, not a credential
	envLocalRetiredRefs   = "CP_AUDIT_LOCAL_RETIRED_KEY_REFS" //nolint:gosec // G101: the name of a SecretRef variable, not a credential
	envRetiredKeyIDs      = "CP_AUDIT_RETIRED_SIGNING_KEY_IDS"
	envRevokedKeyIDs      = "CP_AUDIT_REVOKED_SIGNING_KEY_IDS"
	envArchiveDir         = "CP_AUDIT_ARCHIVE_DIR"
	envMaxLeaves          = "CP_AUDIT_MAX_LEAVES"

	defaultCheckpointInterval = 5 * time.Minute
	defaultVerifyInterval     = time.Hour
	defaultSweepEvery         = 12
	// purgeInterval is fixed rather than configurable. Retention is measured
	// in days and the pass is a single DELETE on an indexed column, so there is
	// nothing to tune -- and one more knob is one more thing that can be set to
	// a value that means "never".
	purgeInterval = time.Hour

	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

var errUsage = errors.New("usage")

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

// deps is everything a subcommand needs. It is built once by wire.
type deps struct {
	cfg     *config.Config
	db      *db.DB
	signer  proof.Signer  // nil when unconfigured (verify tolerates it without checkpoints)
	keys    *proof.KeySet // every key trusted to have signed a checkpoint, active and retired
	archive proof.Archive
	clk     clock.Clock
	log     *slog.Logger
	lookup  func(string) (string, bool)
}

func run(args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	case "run", "checkpoint", "verify", "bundle", "purge":
	default:
		fmt.Fprintf(stderr, "audit-worker: unknown command %q\n", cmd)
		usage(stderr)
		return exitUsage
	}
	if cmd == "bundle" && len(rest) != 1 {
		fmt.Fprintln(stderr, "audit-worker: bundle expects exactly one <fill-id|intent-id>")
		usage(stderr)
		return exitUsage
	}
	if cmd != "bundle" && len(rest) != 0 {
		fmt.Fprintf(stderr, "audit-worker: %s takes no arguments\n", cmd)
		usage(stderr)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := wire(ctx, lookup, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "audit-worker:", err)
		return exitFailure
	}
	defer d.db.Close()

	switch cmd {
	case "run":
		err = cmdRun(ctx, d)
	case "checkpoint":
		err = cmdCheckpoint(ctx, d, stdout)
	case "verify":
		var ok bool
		ok, err = cmdVerify(ctx, d, stdout)
		if err == nil && !ok {
			return exitFailure
		}
	case "bundle":
		err = cmdBundle(ctx, d, rest[0], stdout)
	case "purge":
		err = cmdPurge(ctx, d, stdout)
	}
	if err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintln(stderr, "audit-worker:", err)
			usage(stderr)
			return exitUsage
		}
		fmt.Fprintln(stderr, "audit-worker:", err)
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: audit-worker <command> [args]

  run                  periodic checkpoints (%s, default %s) and full verification
                       (%s, default %s); a full candidate sweep every %s runs (default %d)
  checkpoint           create one checkpoint over every uncovered audit event and exit
  verify               verify hash chains, checkpoint signatures, Merkle roots and archived objects;
                       print the JSON report; exit 1 on any failure
  bundle <id>          print the proof bundle (PART 88) for a fill id or a trade intent id
  purge                one retention pass over login_attempts and exit; needs CP_DATABASE_OPS_URL

env: CP_* (see internal/config); %s (SecretRef to a PEM P-256 key, LOCAL/TEST/DEV only);
     %s (filesystem archive directory, LOCAL/TEST/DEV only); %s (leaves per checkpoint)
key rotation: the active signing key is trusted automatically; keep rotated-out keys trusted with
     %s (KMS key ARNs) or %s (SecretRefs to retired PEM keys),
     and withdraw a key with %s (overrides the others, needs no key material)
`, envCheckpointInterval, defaultCheckpointInterval, envVerifyInterval, defaultVerifyInterval, envSweepEvery, defaultSweepEvery,
		envLocalKeyRef, envArchiveDir, envMaxLeaves, envRetiredKeyIDs, envLocalRetiredRefs, envRevokedKeyIDs)
}

// wire loads configuration and builds every dependency. CP_ENV defaults to
// LOCAL with a warning, mirroring cmd/migrate, so `make verify-audit` works
// on a developer machine; every deployed environment sets it explicitly.
func wire(ctx context.Context, lookup func(string) (string, bool), stderr io.Writer) (*deps, error) {
	if v, ok := lookup(config.EnvVarEnvironment); !ok || strings.TrimSpace(v) == "" {
		fmt.Fprintf(stderr, "audit-worker: WARNING %s is not set; assuming LOCAL\n", config.EnvVarEnvironment)
		inner := lookup
		lookup = func(k string) (string, bool) {
			if k == config.EnvVarEnvironment {
				return string(config.EnvLocal), true
			}
			return inner(k)
		}
	}
	cfg, err := config.Load(ctx, lookup)
	if err != nil {
		return nil, err
	}
	log := observability.NewLogger(cfg.Env, stderr).With("service", "audit-worker", "build_version", cfg.BuildVersion, "env", cfg.Env)
	resolver := config.NewResolver(cfg.Env, lookup)

	dbURL, err := resolver.Resolve(ctx, cfg.Database.AppURL)
	if err != nil {
		return nil, fmt.Errorf("resolve database url: %w", err)
	}
	d, err := db.Open(ctx, db.Config{
		URL: dbURL, AppName: "audit-worker", RequireTLS: cfg.Database.RequireTLS,
		MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns,
		StatementTimeout: cfg.Database.StatementTimeout, LockTimeout: cfg.Database.LockTimeout,
	})
	if err != nil {
		return nil, err
	}
	out := &deps{cfg: cfg, db: d, clk: clock.System(), log: log, lookup: lookup}
	out.signer, err = buildSigner(ctx, cfg, resolver, lookup, log)
	if err != nil {
		d.Close()
		return nil, err
	}
	out.keys, err = buildKeySet(ctx, cfg, resolver, lookup, out.signer, log)
	if err != nil {
		d.Close()
		return nil, err
	}
	out.archive, err = buildArchive(cfg, lookup, log)
	if err != nil {
		d.Close()
		return nil, err
	}
	return out, nil
}

// buildKeySet assembles the keys trusted to have signed a checkpoint: the
// active signing key, every key explicitly retired by an operator, and every
// revoked key id (which overrides the other two and needs no material).
//
// Rotation is why this is a set. A checkpoint signed last quarter is verified
// with last quarter's key, so rotating only requires adding the outgoing key
// id to the retired list; nothing about the existing history changes.
func buildKeySet(ctx context.Context, cfg *config.Config, resolver config.Resolver, lookup func(string) (string, bool), signer proof.Signer, log *slog.Logger) (*proof.KeySet, error) {
	var keys []proof.TrustedKey
	switch s := signer.(type) {
	case nil: // verification-only deployment with no signing key configured
	case *proof.LocalECDSASigner:
		keys = append(keys, s.TrustedKey(proof.KeyActive))
	case *proof.KMSSigner:
		id := cfg.KMS.AuditSigningKeyID
		if strings.HasPrefix(id, "alias/") || strings.Contains(id, ":alias/") {
			log.Warn("the configured KMS signing key is an alias; checkpoints record the key ARN, so the ARN must be listed in "+envRetiredKeyIDs+" or verification will report checkpoint_key_unknown",
				"configured_key_id", id)
		}
		keys = append(keys, s.TrustedKey(id, proof.KeyActive))
	default:
		return nil, fmt.Errorf("unknown signer implementation %T", signer)
	}

	// Retired KMS keys verify through the same KMS client: the private key
	// never left KMS, so nothing but the id is needed here.
	retiredIDs := splitList(lookup, envRetiredKeyIDs)
	if len(retiredIDs) > 0 {
		kmsSigner, ok := signer.(*proof.KMSSigner)
		if !ok {
			return nil, fmt.Errorf("%s names retired KMS keys but no KMS signer is configured (set CP_KMS_AUDIT_SIGNING_KEY_ID)", envRetiredKeyIDs)
		}
		for _, id := range retiredIDs {
			keys = append(keys, kmsSigner.TrustedKey(id, proof.KeyRetired))
		}
	}
	// Retired local keys need their material, so each is a SecretRef to a PEM.
	for _, ref := range splitList(lookup, envLocalRetiredRefs) {
		key, err := loadLocalKey(ctx, cfg, resolver, config.SecretRef(ref), envLocalRetiredRefs)
		if err != nil {
			return nil, err
		}
		s, err := proof.NewLocalECDSASigner(cfg.Env, key)
		if err != nil {
			return nil, err
		}
		keys = append(keys, s.TrustedKey(proof.KeyRetired))
	}
	keys = dedupeKeys(keys, log)
	// Revocation is an override: a revoked id is refused even if it is also
	// the active or a retired key.
	revoked := splitList(lookup, envRevokedKeyIDs)
	if len(revoked) > 0 {
		byID := map[string]bool{}
		for _, id := range revoked {
			byID[id] = true
		}
		kept := keys[:0]
		for _, k := range keys {
			if !byID[k.KeyID] {
				kept = append(kept, k)
			}
		}
		keys = kept
		for _, id := range revoked {
			keys = append(keys, proof.RevokedKey(id))
		}
	}
	ks, err := proof.NewKeySet(keys...)
	if err != nil {
		return nil, err
	}
	if ks.Len() > 0 {
		log.Info("trusted audit signing keys", "key_ids", ks.KeyIDs(), "count", ks.Len(), "revoked", revoked)
	}
	return ks, nil
}

// dedupeKeys drops a duplicate id, keeping the first (stronger) status.
func dedupeKeys(keys []proof.TrustedKey, log *slog.Logger) []proof.TrustedKey {
	seen := make(map[string]bool, len(keys))
	out := keys[:0]
	for _, k := range keys {
		if seen[k.KeyID] {
			log.Debug("trusted key listed more than once; keeping the first entry", "key_id", k.KeyID, "dropped_status", k.Status)
			continue
		}
		seen[k.KeyID] = true
		out = append(out, k)
	}
	return out
}

// splitList reads a comma-separated variable, trimming blanks.
func splitList(lookup func(string) (string, bool), name string) []string {
	raw, _ := lookup(name)
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadLocalKey resolves ref and parses the PEM key it points at. Errors never
// echo key material.
func loadLocalKey(ctx context.Context, cfg *config.Config, resolver config.Resolver, ref config.SecretRef, name string) (*ecdsa.PrivateKey, error) {
	if cfg.Env.IsProductionLike() {
		return nil, fmt.Errorf("%s is set but local signing keys are not permitted in %s", name, cfg.Env)
	}
	if err := ref.ValidateFor(cfg.Env); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	pemText, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	key, err := proof.ParseLocalKeyPEM(pemText)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return key, nil
}

// buildSigner returns the KMS signer, or the local signer in LOCAL/TEST/DEV
// when a PEM SecretRef is configured, or nil when neither is configured.
func buildSigner(ctx context.Context, cfg *config.Config, resolver config.Resolver, lookup func(string) (string, bool), log *slog.Logger) (proof.Signer, error) {
	localRef, _ := lookup(envLocalKeyRef)
	localRef = strings.TrimSpace(localRef)
	if localRef != "" && !cfg.Env.IsProductionLike() {
		key, err := loadLocalKey(ctx, cfg, resolver, config.SecretRef(localRef), envLocalKeyRef)
		if err != nil {
			return nil, err
		}
		s, err := proof.NewLocalECDSASigner(cfg.Env, key)
		if err != nil {
			return nil, err
		}
		log.Warn("audit checkpoints are signed with a LOCAL test key, not KMS", "signer", s.Kind(), "key_id", s.KeyID())
		return s, nil
	}
	if localRef != "" {
		return nil, fmt.Errorf("%s is set but local signing keys are not permitted in %s", envLocalKeyRef, cfg.Env)
	}
	if cfg.KMS.AuditSigningKeyID == "" {
		log.Warn("no audit signer configured: checkpoint/run will fail and verify only succeeds while no checkpoint exists",
			"hint", "set CP_KMS_AUDIT_SIGNING_KEY_ID or (LOCAL/TEST/DEV) "+envLocalKeyRef)
		return nil, nil
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.KMS.Region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	s, err := proof.NewKMSSigner(kms.NewFromConfig(awsCfg), cfg.KMS.AuditSigningKeyID)
	if err != nil {
		return nil, err
	}
	log.Info("audit checkpoints are signed with KMS", "key_id", cfg.KMS.AuditSigningKeyID, "region", cfg.KMS.Region)
	return s, nil
}

// buildArchive returns the filesystem archive when CP_AUDIT_ARCHIVE_DIR is
// set (LOCAL/TEST/DEV), or nil until the Object Lock archive is wired.
func buildArchive(cfg *config.Config, lookup func(string) (string, bool), log *slog.Logger) (proof.Archive, error) {
	dir, _ := lookup(envArchiveDir)
	dir = strings.TrimSpace(dir)
	if dir == "" {
		log.Warn("no audit archive configured: checkpoint/run will fail and verify only succeeds while no checkpoint exists",
			"hint", "the S3 Object Lock archive is wired by the composition root; "+envArchiveDir+" selects a filesystem archive in LOCAL/TEST/DEV",
			"audit_bucket", cfg.Archive.AuditBucket)
		return nil, nil
	}
	a, err := proof.NewDirArchive(cfg.Env, dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envArchiveDir, err)
	}
	log.Warn("audit checkpoints are archived to a filesystem directory, not an Object Lock bucket", "dir", a.Root())
	return a, nil
}

func (d *deps) checkpointer() (*proof.Checkpointer, error) {
	if d.signer == nil {
		return nil, errors.New("no signer configured: set CP_KMS_AUDIT_SIGNING_KEY_ID or (LOCAL/TEST/DEV) " + envLocalKeyRef)
	}
	if d.archive == nil {
		return nil, errors.New("no archive configured: wire the Object Lock archive or (LOCAL/TEST/DEV) set " + envArchiveDir)
	}
	opts := proof.CheckpointerOptions{BuildVersion: d.cfg.BuildVersion}
	if v, ok := d.lookup(envMaxLeaves); ok && strings.TrimSpace(v) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%s: invalid positive integer %q", envMaxLeaves, v)
		}
		opts.MaxLeaves = n
	}
	if days := d.cfg.Retention.SecurityAuditDays; days > 0 {
		retention := time.Duration(days) * 24 * time.Hour
		opts.Retention = &retention
	}
	return proof.NewCheckpointer(d.db, d.signer, d.archive, d.clk, opts)
}

func (d *deps) verifier() (*proof.Verifier, error) {
	return proof.NewVerifier(d.keys, d.archive, d.clk, proof.VerifierOptions{BuildVersion: d.cfg.BuildVersion})
}

// warnIfUntrusted reports a freshly created checkpoint whose signing key is
// not trusted for verification: the next verify would report
// checkpoint_key_unknown, and saying so now names the cause (typically a KMS
// alias configured where the ARN belongs) instead of leaving it to look like
// tampering later.
func (d *deps) warnIfUntrusted(cp proof.Checkpoint) {
	status, ok := d.keys.Status(cp.SigningKeyID)
	switch {
	case !ok:
		d.log.Error("the key that signed this checkpoint is not in the trusted key set; verification will report checkpoint_key_unknown",
			"signing_key_id", cp.SigningKeyID, "checkpoint_seq", cp.Seq, "trusted", d.keys.KeyIDs(),
			"hint", "trust the key id the row records (the KMS key ARN, not an alias)")
	case status != proof.KeyActive:
		d.log.Warn("this checkpoint was signed with a key that is not active in the trusted key set",
			"signing_key_id", cp.SigningKeyID, "status", status, "checkpoint_seq", cp.Seq)
	}
}

func cmdCheckpoint(ctx context.Context, d *deps, stdout io.Writer) error {
	cp, err := d.checkpointer()
	if err != nil {
		return err
	}
	for {
		res, err := cp.RunFull(ctx)
		if err != nil {
			return err
		}
		if res.Created {
			d.warnIfUntrusted(res.Checkpoint)
		}
		if err := printJSON(stdout, checkpointSummary(res)); err != nil {
			return err
		}
		if !res.Truncated {
			return nil
		}
	}
}

func cmdVerify(ctx context.Context, d *deps, stdout io.Writer) (bool, error) {
	v, err := d.verifier()
	if err != nil {
		return false, err
	}
	rep, err := v.VerifyAll(ctx, d.db)
	if err != nil {
		return false, err
	}
	if err := printJSON(stdout, rep); err != nil {
		return false, err
	}
	if !rep.OK && rep.FirstFailure != nil {
		fmt.Fprintln(stdout, "verify: FAILED", rep.FirstFailure.String())
	} else if rep.OK {
		fmt.Fprintf(stdout, "verify: ok (%d streams, %d events, %d checkpoints covering %d events)\n", rep.Streams, rep.CheckedEvents, rep.CheckedCheckpoints, rep.CoveredEvents)
	}
	return rep.OK, nil
}

func cmdBundle(ctx context.Context, d *deps, idText string, stdout io.Writer) error {
	ref, err := proof.ResolveRef(ctx, d.db, idText)
	if err != nil {
		return err
	}
	b, err := proof.NewBundler(d.clk, d.cfg.BuildVersion)
	if err != nil {
		return err
	}
	bundle, err := b.Bundle(ctx, d.db, ref)
	if err != nil {
		return err
	}
	return printJSON(stdout, bundle)
}

// cmdRun loops until ctx is cancelled: a checkpoint every checkpoint
// interval (a full sweep on the first run and every sweepEvery runs, looping
// immediately while a run reports Truncated) and a full verification every
// verify interval. Verification failures are logged at error level and
// recorded in audit_verification_runs; the worker keeps running so the
// failure keeps being reported.
// cmdPurge deletes login_attempts rows whose expiry passed more than
// CP_RETENTION_LOGIN_ATTEMPT_DAYS ago and reports how many went.
//
// The rows hold a plaintext OIDC nonce and PKCE code_verifier, and 00641 said
// they were "purged by the ops role instead" while nothing anywhere deleted one
// (F-79). The secrets are single-use and the durable record of a login is a
// security_events row, so this loses no investigative trail.
func cmdPurge(ctx context.Context, d *deps, out io.Writer) error {
	pool, err := d.opsPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	n, err := purgeOnce(ctx, d, pool)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "purged %d expired login attempt(s) older than %d day(s)\n", n, d.cfg.Retention.LoginAttemptDays)
	return nil
}

// purgeOnce is the single pass both the command and the run loop perform.
func purgeOnce(ctx context.Context, d *deps, pool identity.Purger) (int64, error) {
	days := d.cfg.Retention.LoginAttemptDays
	if days <= 0 {
		return 0, fmt.Errorf("CP_RETENTION_LOGIN_ATTEMPT_DAYS is %d: a retention pass with no retention would delete every login attempt ever made", days)
	}
	n, err := identity.PurgeLoginAttempts(ctx, pool, d.clk.Now(), time.Duration(days)*24*time.Hour)
	if err != nil {
		return 0, err
	}
	d.log.Info("login attempts purged", "rows", n, "retention_days", days)
	return n, nil
}

// opsPool opens the cp_ops connection. It is a separate pool, opened only by
// the commands that need it, because cp_app is deliberately refused DELETE on
// login_attempts: an attacker holding the application credential must not be
// able to erase the record of the logins they attempted.
//
// It refuses rather than skipping when the URL is unset. A retention pass that
// quietly does nothing is the shape of defect this repository keeps finding --
// a control that reports success having run nothing -- and the operator who
// deployed this binary asked for the purge by deploying it.
func (d *deps) opsPool(ctx context.Context) (*db.DB, error) {
	if d.cfg.Database.OpsURL == "" {
		return nil, errors.New("CP_DATABASE_OPS_URL is not set: the retention pass needs the cp_ops role, " +
			"because cp_app holds no DELETE on login_attempts")
	}
	url, err := config.NewResolver(d.cfg.Env, d.lookup).Resolve(ctx, d.cfg.Database.OpsURL)
	if err != nil {
		return nil, fmt.Errorf("resolve ops database url: %w", err)
	}
	return db.Open(ctx, db.Config{
		URL: url, AppName: "audit-worker-ops", RequireTLS: d.cfg.Database.RequireTLS,
		MaxConns: 2, MinConns: 0,
		StatementTimeout: d.cfg.Database.StatementTimeout, LockTimeout: d.cfg.Database.LockTimeout,
	})
}

func cmdRun(ctx context.Context, d *deps) error {
	cpInterval, err := durationVar(d.lookup, envCheckpointInterval, defaultCheckpointInterval)
	if err != nil {
		return err
	}
	verifyInterval, err := durationVar(d.lookup, envVerifyInterval, defaultVerifyInterval)
	if err != nil {
		return err
	}
	sweepEvery := defaultSweepEvery
	if v, ok := d.lookup(envSweepEvery); ok && strings.TrimSpace(v) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			return fmt.Errorf("%s: invalid positive integer %q", envSweepEvery, v)
		}
		sweepEvery = n
	}
	cp, err := d.checkpointer()
	if err != nil {
		return err
	}
	v, err := d.verifier()
	if err != nil {
		return err
	}
	// The ops pool is opened before the first tick, not at the first purge, so
	// a missing or wrong CP_DATABASE_OPS_URL is a startup failure rather than a
	// daily log line nobody reads. A retention pass that never runs is the
	// state this worker was given the command to end (F-79).
	opsPool, err := d.opsPool(ctx)
	if err != nil {
		return err
	}
	defer opsPool.Close()
	d.log.Info("audit worker starting", "checkpoint_interval", cpInterval, "verify_interval", verifyInterval,
		"sweep_every", sweepEvery, "purge_interval", purgeInterval, "login_attempt_retention_days", d.cfg.Retention.LoginAttemptDays)

	checkpointTick := time.NewTicker(cpInterval)
	defer checkpointTick.Stop()
	verifyTick := time.NewTicker(verifyInterval)
	defer verifyTick.Stop()
	purgeTick := time.NewTicker(purgeInterval)
	defer purgeTick.Stop()

	runs := 0
	doCheckpoint := func() {
		full := runs%sweepEvery == 0
		runs++
		for {
			var (
				res proof.CheckpointResult
				err error
			)
			if full {
				res, err = cp.RunFull(ctx)
			} else {
				res, err = cp.Run(ctx)
			}
			if err != nil {
				if ctx.Err() == nil {
					d.log.Error("checkpoint run failed", "error", err, "full", full)
				}
				return
			}
			logCheckpoint(d.log, res, full)
			if res.Created {
				d.warnIfUntrusted(res.Checkpoint)
			}
			if !res.Truncated {
				return
			}
			full = false
		}
	}
	doVerify := func() {
		rep, err := v.VerifyAll(ctx, d.db)
		if err != nil {
			if ctx.Err() == nil {
				d.log.Error("verification run failed to complete", "error", err)
			}
			return
		}
		if rep.OK {
			d.log.Info("audit history verified", "run_id", rep.RunID, "streams", rep.Streams, "events", rep.CheckedEvents,
				"checkpoints", rep.CheckedCheckpoints, "covered_events", rep.CoveredEvents)
			return
		}
		d.log.Error("AUDIT HISTORY DOES NOT VERIFY", "run_id", rep.RunID, "failure", rep.FirstFailure.String(), "kind", rep.FirstFailure.Kind)
	}

	doCheckpoint()
	doVerify()
	for {
		select {
		case <-ctx.Done():
			d.log.Info("audit worker stopping")
			return nil
		case <-checkpointTick.C:
			doCheckpoint()
		case <-verifyTick.C:
			doVerify()
		case <-purgeTick.C:
			// A failed purge is logged and the loop continues: retention is
			// not integrity, and stopping the checkpointer because a DELETE
			// failed would trade the thing this worker exists for against the
			// thing it was lately given to do.
			if _, err := purgeOnce(ctx, d, opsPool); err != nil {
				d.log.Error("retention pass failed", "error", err)
			}
		}
	}
}

func logCheckpoint(log *slog.Logger, res proof.CheckpointResult, full bool) {
	switch {
	case res.Created:
		log.Info("checkpoint created", "seq", res.Checkpoint.Seq, "id", res.Checkpoint.ID, "leaves", res.Checkpoint.LeafCount,
			"streams", len(res.Checkpoint.StreamsCovered), "signer", res.Checkpoint.Signer, "key_id", res.Checkpoint.SigningKeyID,
			"archive_uri", res.Checkpoint.ArchiveURI, "truncated", res.Truncated, "full", full)
	default:
		log.Debug("no checkpoint created", "reason", res.Skipped, "full", full)
	}
}

type checkpointSummaryJSON struct {
	Created    bool             `json:"created"`
	Skipped    proof.SkipReason `json:"skipped,omitempty"`
	Truncated  bool             `json:"truncated"`
	ID         string           `json:"id,omitempty"`
	Seq        int64            `json:"seq,omitempty"`
	LeafCount  int              `json:"leaf_count,omitempty"`
	Streams    int              `json:"streams,omitempty"`
	MerkleRoot []byte           `json:"merkle_root,omitempty"`
	Signer     proof.SignerKind `json:"signer,omitempty"`
	KeyID      string           `json:"signing_key_id,omitempty"`
	ArchiveURI string           `json:"archive_uri,omitempty"`
}

func checkpointSummary(res proof.CheckpointResult) checkpointSummaryJSON {
	s := checkpointSummaryJSON{Created: res.Created, Skipped: res.Skipped, Truncated: res.Truncated}
	if res.Created {
		c := res.Checkpoint
		s.ID, s.Seq, s.LeafCount, s.Streams = c.ID.String(), c.Seq, c.LeafCount, len(c.StreamsCovered)
		s.MerkleRoot, s.Signer, s.KeyID, s.ArchiveURI = c.MerkleRoot, c.Signer, c.SigningKeyID, c.ArchiveURI
	}
	return s
}

func durationVar(lookup func(string) (string, bool), name string, def time.Duration) (time.Duration, error) {
	v, ok := lookup(name)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: invalid positive duration %q", name, v)
	}
	return d, nil
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
