package delegation

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

func unitSigner(t *testing.T, seed [32]byte) *DelegatedSigner {
	t.Helper()
	s, err := NewDelegatedSigner(seed, SignerConfig{Environment: config.EnvTest, Clock: clock.NewFake(unitNow),
		Policy: DefaultSigningPolicy(), Fees: func(devnet.ChainID) (uint64, bool) { return 10, true }})
	require.NoError(t, err)
	return s
}

// Keys are derived, not stored: the same seed gives the same wallet for an
// account and chain on every run (a restart keeps every key), and nothing
// else does.
func TestDelegatedKeysAreDerivedPerAccountAndChain(t *testing.T) {
	a := accounts.AccountID(id.New[id.Any]())
	b := accounts.AccountID(id.New[id.Any]())
	s1 := unitSigner(t, useed("master"))
	s2 := unitSigner(t, useed("master"))
	other := unitSigner(t, useed("another master"))
	assert.Equal(t, s1.PublicKey(a, devnethost.ChainSolana), s2.PublicKey(a, devnethost.ChainSolana), "a restart keeps the key")
	assert.NotEqual(t, s1.PublicKey(a, devnethost.ChainSolana), s1.PublicKey(b, devnethost.ChainSolana), "one key per account")
	assert.NotEqual(t, s1.PublicKey(a, devnethost.ChainSolana), s1.PublicKey(a, devnethost.ChainEthereum), "one key per chain")
	assert.NotEqual(t, s1.PublicKey(a, devnethost.ChainSolana), other.PublicKey(a, devnethost.ChainSolana))
}

func TestTheSignerRefusesToStartWithoutWhatItNeeds(t *testing.T) {
	_, err := NewDelegatedSigner([32]byte{}, SignerConfig{Environment: config.EnvTest, Clock: clock.NewFake(unitNow),
		Policy: DefaultSigningPolicy(), Fees: func(devnet.ChainID) (uint64, bool) { return 10, true }})
	require.Error(t, err, "a zero master seed")
	_, err = NewDelegatedSigner(useed("m"), SignerConfig{Environment: config.EnvTest, Policy: DefaultSigningPolicy(),
		Fees: func(devnet.ChainID) (uint64, bool) { return 10, true }})
	require.Error(t, err, "no clock")
	_, err = NewDelegatedSigner(useed("m"), SignerConfig{Environment: config.EnvTest, Clock: clock.NewFake(unitNow),
		Policy: SigningPolicy{}, Fees: func(devnet.ChainID) (uint64, bool) { return 10, true }})
	require.Error(t, err, "no policy")
}

// No formatting of a signer reaches its key material: not the master seed,
// not a derived private key.
func TestSignersNeverFormatTheirSecrets(t *testing.T) {
	seed := useed("master")
	s := unitSigner(t, seed)
	v := NewVenueSigner(useed("venue"))
	secrets := []string{hex.EncodeToString(seed[:]), fmt.Sprintf("%x", useed("venue")), string(seed[:])}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%X", "%q", "%p", "%d"} {
		for _, out := range []string{fmt.Sprintf(verb, s), fmt.Sprintf(verb, v), fmt.Sprintf(verb, *s), fmt.Sprintf(verb, *v)} {
			for _, secret := range secrets {
				assert.False(t, strings.Contains(out, secret), "verb %s leaked a secret", verb)
			}
		}
	}
	assert.Equal(t, "DelegatedSigner", s.String())
	assert.Equal(t, "VenueSigner("+v.PublicKey().String()+")", v.String())
}

func TestTheVenueCoSignsOnlyThePlannedTransaction(t *testing.T) {
	e := unitSwapExpectation(t)
	v := NewVenueSigner(useed("venue"))
	require.Equal(t, ukey("venue"), v.PublicKey())
	m := plannedSwap(t, e)
	sig, err := v.CoSign(e.Plan, e.PlanHash, e.Blockhash, m)
	require.NoError(t, err)
	b, err := m.Bytes()
	require.NoError(t, err)
	assert.True(t, devnet.Verify(v.PublicKey(), b, sig))

	for name, mutate := range map[string]func(*devnet.Message){
		"a smaller payment to the venue": func(m *devnet.Message) {
			p := m.Instructions[0].(devnet.TokenTransfer)
			p.Amount--
			m.Instructions[0] = p
		},
		"a larger delivery from the venue": func(m *devnet.Message) {
			r := m.Instructions[1].(devnet.TokenTransfer)
			r.Amount++
			m.Instructions[1] = r
		},
		"the venue pays the fee": func(m *devnet.Message) { m.FeePayer = ukey("venue") },
		"another blockhash":      func(m *devnet.Message) { m.RecentBlockhash = uhash("other") },
	} {
		mm := m
		mm.Instructions = append([]devnet.Instruction(nil), m.Instructions...)
		mutate(&mm)
		_, err := v.CoSign(e.Plan, e.PlanHash, e.Blockhash, mm)
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeSigningRejected, errs.CodeOf(err), name)
	}
	// Another venue's plan is not this venue's to sign.
	other := NewVenueSigner(useed("other venue"))
	_, err = other.CoSign(e.Plan, e.PlanHash, e.Blockhash, m)
	require.Error(t, err)
	// A plan whose stored hash does not match is refused.
	_, err = v.CoSign(e.Plan, uhashBytes("x"), e.Blockhash, m)
	require.Error(t, err)
}

func uhashBytes(label string) [32]byte { return [32]byte(uhash(label)) }

func TestSigningPolicyHashIsStableAndValidated(t *testing.T) {
	p := DefaultSigningPolicy()
	require.NoError(t, p.Validate())
	assert.Equal(t, p.Hash(), DefaultSigningPolicy().Hash())
	q := p
	q.MaxFeePerTx++
	assert.NotEqual(t, p.Hash(), q.Hash())
	bad := p
	bad.AllowedChains = []devnet.ChainID{"sbx-other"}
	assert.Error(t, bad.Validate())
}
