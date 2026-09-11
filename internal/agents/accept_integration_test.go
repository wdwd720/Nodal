//go:build integration

package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/strategy"
)

// The acceptance route (F-255, D-128).
//
// Every test here exercises the act goal §18 requires and nothing in this build
// could perform: a person reading a compiled strategy and approving it. The
// fixture's own version arrives ACCEPTED, so these build their own COMPILED one
// and move it.

// acceptFixture adds a compiled, unaccepted version to the standard fixture and
// wires the strategy service with an audit writer, which Accept requires.
type acceptFixture struct {
	*fixture
	versionNo int
	irHash    string
	versionID string
}

func newAcceptFixture(t *testing.T) *acceptFixture {
	t.Helper()
	f := newFixture(t)
	st, err := NewStrategyService(StrategyDeps{
		DB: testDB, Clock: f.clk, CompilerVersion: "itest", Environment: "TEST",
		Audit: audit.NewWriterWithBuildVersion("itest"),
	})
	require.NoError(t, err)
	f.strategies = st

	af := &acceptFixture{fixture: f, versionNo: 2, versionID: newUUID()}
	af.irHash = "c0ffee" + strings.Repeat("00", 29) // 32 bytes, lowercase hex
	_, err = testDB.Exec(context.Background(), `
		INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set,
		    status, source_kind, source_hash, compiler_version, risk_policy_version, risk_policy_hash,
		    model_budget, data_budget, envelope_requirements, human_readable, built_at)
		VALUES ($1, $2, $3, 1, '{"schema_version":1}'::jsonb, decode($4, 'hex'), ARRAY['READ_MARKET_DATA']::text[],
		        'COMPILED', 'NATURAL_LANGUAGE', $5, 'c/1', 'risk/v1', $6,
		        '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'a compiled strategy, in words', now())`,
		af.versionID, f.strategyID, af.versionNo, af.irHash, bytes32("src2"), bytes32("risk"))
	require.NoError(t, err)
	return af
}

func (af *acceptFixture) accept(ctx context.Context, version int, hash string) (StrategyVersion, error) {
	return af.strategies.Accept(ctx, AcceptRequest{
		StrategyID: af.strategyID, Version: version, IRHashHex: hash,
		RequestID: newUUID(), CorrelationID: "itest",
	})
}

// TestIntegration_AcceptingIsAPersonsActAndItIsRecorded.
func TestIntegration_AcceptingIsAPersonsActAndItIsRecorded(t *testing.T) {
	af := newAcceptFixture(t)
	ctx := af.owner()

	v, err := af.accept(ctx, af.versionNo, af.irHash)
	require.NoError(t, err)
	assert.Equal(t, strategy.StatusAccepted, v.Status)
	assert.Equal(t, af.userID, v.AcceptedByUserID, "the record names the person")
	require.NotNil(t, v.AcceptedAt)
	assert.Equal(t, af.clk.Now().UTC(), v.AcceptedAt.UTC())

	// The row, not just the answer. 00500's CHECK pairs the status with the two
	// columns, so a row in one of these states and not the other cannot exist.
	var status, by string
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT status, accepted_by_user_id::text FROM strategy_versions WHERE id = $1::uuid`, af.versionID).
		Scan(&status, &by))
	assert.Equal(t, strategy.StatusAccepted, status)
	assert.Equal(t, af.userID, by)

	// And the trail, on the owner's own account stream.
	var action, stream string
	var payload []byte
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT action, stream, payload FROM audit_events
		  WHERE resource_id = $1 AND action = $2 ORDER BY stream_seq DESC LIMIT 1`,
		af.versionID, AcceptAction).Scan(&action, &stream, &payload))
	assert.Equal(t, audit.AccountStream(af.accountID), stream)
	var body map[string]any
	require.NoError(t, json.Unmarshal(payload, &body))
	assert.Equal(t, af.irHash, body["ir_hash"], "the audit row names the document that was approved")
}

// TestIntegration_AnAgentBecomesCreatableOnlyAfterAcceptance is F-255 in one
// case: the refusal that made an agent unreachable, and the act that lifts it.
func TestIntegration_AnAgentBecomesCreatableOnlyAfterAcceptance(t *testing.T) {
	af := newAcceptFixture(t)
	ctx := af.owner()

	_, err := af.svc.Create(ctx, CreateRequest{
		AccountID: af.accountID, StrategyID: af.strategyID, StrategyVersionID: af.versionID,
		Name: "before acceptance", Level: agentauthority.LevelRecommendation,
		Limits: fixtureLimits(t, af.assetID),
	})
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeInvalidStateTransition, e.Code)

	_, err = af.accept(ctx, af.versionNo, af.irHash)
	require.NoError(t, err)

	v, err := af.svc.Create(ctx, CreateRequest{
		AccountID: af.accountID, StrategyID: af.strategyID, StrategyVersionID: af.versionID,
		Name: "after acceptance", Level: agentauthority.LevelRecommendation,
		Limits: fixtureLimits(t, af.assetID),
	})
	require.NoError(t, err)
	assert.Equal(t, af.versionID, v.Agent.StrategyVersionID)
}

// TestIntegration_AcceptingNeedsTheHashThatWasRead: this is the field that
// makes "accept" mean "accept THIS document".
func TestIntegration_AcceptingNeedsTheHashThatWasRead(t *testing.T) {
	af := newAcceptFixture(t)
	ctx := af.owner()

	_, err := af.accept(ctx, af.versionNo, strings.Repeat("ab", 32))
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeConflict, e.Code)

	_, err = af.accept(ctx, af.versionNo, "")
	require.Error(t, err)
	e, ok = errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeValidationFailed, e.Code)

	// Nothing moved.
	var status string
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT status FROM strategy_versions WHERE id = $1::uuid`, af.versionID).Scan(&status))
	assert.Equal(t, strategy.StatusCompiled, status)
}

// TestIntegration_ASecondAcceptanceIsAReplay: asking for a state the row is
// already in is not an error, and a lost response must not look like one.
func TestIntegration_ASecondAcceptanceIsAReplay(t *testing.T) {
	af := newAcceptFixture(t)
	ctx := af.owner()

	first, err := af.accept(ctx, af.versionNo, af.irHash)
	require.NoError(t, err)
	af.clk.Advance(time.Minute)
	second, err := af.accept(ctx, af.versionNo, af.irHash)
	require.NoError(t, err)

	assert.Equal(t, first.AcceptedByUserID, second.AcceptedByUserID)
	require.NotNil(t, second.AcceptedAt)
	assert.Equal(t, first.AcceptedAt.UTC(), second.AcceptedAt.UTC(), "the instant is the first one")

	// One audit event, not two: nothing happened the second time.
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE resource_id = $1 AND action = $2`,
		af.versionID, AcceptAction).Scan(&n))
	assert.Equal(t, 1, n)
}

// TestIntegration_OnlyTheOwnerAccepts: a stranger is told the strategy does not
// exist, which is the answer Get already gives, and an operator's read
// capability does not substitute for the owner's approval.
func TestIntegration_OnlyTheOwnerAccepts(t *testing.T) {
	af := newAcceptFixture(t)

	_, err := af.accept(af.stranger(), af.versionNo, af.irHash)
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeNotFound, e.Code, "not yours is not found")

	_, err = af.accept(af.operator(), af.versionNo, af.irHash)
	require.Error(t, err)
	e, ok = errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeForbidden, e.Code, "approving a strategy is not a read")

	var status string
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT status FROM strategy_versions WHERE id = $1::uuid`, af.versionID).Scan(&status))
	assert.Equal(t, strategy.StatusCompiled, status)
}

// TestIntegration_AVersionThatIsNotCompiledCannotBeAccepted.
func TestIntegration_AVersionThatIsNotCompiledCannotBeAccepted(t *testing.T) {
	af := newAcceptFixture(t)

	// The fixture's version 1 is already ACCEPTED by somebody, with a hash of
	// its own. Accepting a REVOKED one is the case that must refuse.
	_, err := testDB.Exec(context.Background(),
		`UPDATE strategy_versions SET status = 'REVOKED', revoked_at = now() WHERE id = $1::uuid`, af.versionID)
	require.NoError(t, err)

	_, err = af.accept(af.owner(), af.versionNo, af.irHash)
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeInvalidStateTransition, e.Code)
}

// TestIntegration_AVersionThatDoesNotExistIsNotFound.
func TestIntegration_AVersionThatDoesNotExistIsNotFound(t *testing.T) {
	af := newAcceptFixture(t)
	_, err := af.accept(af.owner(), 99, af.irHash)
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeNotFound, e.Code)
}
