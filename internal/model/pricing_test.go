package model_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/model"
	"github.com/nodal/controlplane/internal/model/modeltest"
)

// TestNoFloatingPointInModel: cost is money, so no float may appear in the
// production sources of this package (goal PART 224, CONVENTIONS #1).
func TestNoFloatingPointInModel(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoError(t, perr)
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				assert.NotContains(t, []string{"float32", "float64"}, x.Name,
					"%s:%d uses %s", name, fset.Position(x.Pos()).Line, x.Name)
			case *ast.BasicLit:
				if x.Kind == token.FLOAT || x.Kind == token.IMAG {
					t.Errorf("%s:%d has the float literal %s", name, fset.Position(x.Pos()).Line, x.Value)
				}
			}
			return true
		})
	}
	require.Greater(t, checked, 0)
}

// TestPriceTable_Cost pins the arithmetic against hand-computed values from
// the published rates.
func TestPriceTable_Cost(t *testing.T) {
	t.Parallel()
	table := model.DefaultPriceTable()

	// Opus 5: $5/MTok input, $25/MTok output.
	// 1,000,000 input + 1,000,000 output = 500c + 2500c = $30.00.
	cost, err := table.Cost("claude-opus-5", model.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	require.NoError(t, err)
	assert.Equal(t, "30.00", cost.String())

	// A tenth of that is exactly $3.00.
	cost, err = table.Cost("claude-opus-5", model.Usage{InputTokens: 100_000, OutputTokens: 100_000})
	require.NoError(t, err)
	assert.Equal(t, "3.00", cost.String())

	// Cache reads are billed at $0.50/MTok, writes at $6.25/MTok.
	cost, err = table.Cost("claude-opus-5", model.Usage{CacheReadInputTokens: 1_000_000, CacheCreationInputTokens: 1_000_000})
	require.NoError(t, err)
	assert.Equal(t, "6.75", cost.String())

	// Zero usage costs nothing.
	cost, err = table.Cost("claude-opus-5", model.Usage{})
	require.NoError(t, err)
	assert.True(t, cost.IsZero())
}

// TestPriceTable_RoundsUp: a fractional cent is charged, never dropped, so
// a budget cannot be overspent by accumulated rounding.
func TestPriceTable_RoundsUp(t *testing.T) {
	t.Parallel()
	table := model.DefaultPriceTable()
	// 1 input token on Opus 5 is 500/1,000,000 of a cent: rounds up to 1.
	cost, err := table.Cost("claude-opus-5", model.Usage{InputTokens: 1})
	require.NoError(t, err)
	assert.Equal(t, int64(1), cost.Minor(), "a fraction of a cent still costs a cent")

	// 2000 input tokens is exactly 1 cent, no rounding applied.
	cost, err = table.Cost("claude-opus-5", model.Usage{InputTokens: 2000})
	require.NoError(t, err)
	assert.Equal(t, int64(1), cost.Minor())

	// 2001 tokens crosses into the next cent.
	cost, err = table.Cost("claude-opus-5", model.Usage{InputTokens: 2001})
	require.NoError(t, err)
	assert.Equal(t, int64(2), cost.Minor())
}

// TestPriceTable_UnpricedModelIsAnError: a model we cannot price is a model
// we cannot budget for. Treating it as free would defeat the budget.
func TestPriceTable_UnpricedModelIsAnError(t *testing.T) {
	t.Parallel()
	table := model.DefaultPriceTable()
	_, err := table.Cost("some-unknown-model", model.Usage{InputTokens: 1000})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "some-unknown-model")

	_, err = table.Cost("claude-opus-5", model.Usage{InputTokens: -1})
	require.Error(t, err, "negative usage is rejected")
}

func TestPriceTable_Validate(t *testing.T) {
	t.Parallel()
	require.NoError(t, model.DefaultPriceTable().Validate())

	require.Error(t, model.PriceTable{}.Validate(), "an empty table cannot price a call")

	negative := model.PriceTable{"m": {InputCentsPerMTok: -1}}
	require.Error(t, negative.Validate())

	assert.Equal(t, []string{
		"claude-fable-5-1", "claude-haiku-4-5", "claude-haiku-4-5-20251001", "claude-opus-5", "claude-sonnet-5",
	}, model.DefaultPriceTable().Models(), "models are listed in sorted order")
}

// TestEstimateCost_IsPessimistic: the pre-call estimate assumes the model
// emits its whole output allowance, so a budget check cannot be passed by a
// call that then overspends.
func TestEstimateCost_IsPessimistic(t *testing.T) {
	t.Parallel()
	table := model.DefaultPriceTable()
	estimate, err := table.EstimateCost("claude-opus-5", 10_000, 16_000)
	require.NoError(t, err)

	actual, err := table.Cost("claude-opus-5", model.Usage{InputTokens: 10_000, OutputTokens: 500})
	require.NoError(t, err)
	assert.Greater(t, estimate.Minor(), actual.Minor(), "the estimate bounds a typical actual cost")

	assert.Equal(t, int64(0), model.EstimateInputTokens(""))
	assert.Equal(t, int64(1), model.EstimateInputTokens("abc"), "token estimate rounds up")
	assert.Equal(t, int64(1), model.EstimateInputTokens("abcd"))
	assert.Equal(t, int64(2), model.EstimateInputTokens("abcde"))
}

// TestFake_RejectedOutsideSafeEnvironments: a scripted model must be unable
// to exist where it could reach real money.
func TestFake_RejectedOutsideSafeEnvironments(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		f, err := modeltest.New(env)
		require.NoError(t, err, "%s must allow the fake", env)
		assert.NotNil(t, f)
	}
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd, config.Environment("OTHER"), config.Environment("")} {
		_, err := modeltest.New(env)
		require.Error(t, err, "%s must refuse the fake", env)
		assert.ErrorIs(t, err, model.ErrFakeNotAllowed)
	}
	assert.Panics(t, func() { modeltest.MustNew(config.EnvProd) })
}
