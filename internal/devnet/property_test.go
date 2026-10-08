package devnet

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
)

// ---------------------------------------------------------------------------
// TestProp_TokenConservation
// ---------------------------------------------------------------------------

const convOwners = 4

// Token-conservation operations.
const (
	opTransfer         = iota // TOKEN_TRANSFER, may exceed the balance
	opMintTo                  // TOKEN_MINT_TO, sometimes by the wrong authority
	opBurn                    // TOKEN_BURN, may exceed the balance
	opTransferThenBurn        // two token instructions in one transaction
	opNativeTransfer          // SYSTEM_TRANSFER
	opForeignSigner           // an owner's slot signed by another key: rejected
	opMissingSigner           // the owner's signature absent: rejected
	opStaleBlockhash          // a blockhash never produced: rejected
	opResubmit                // an earlier transaction sent again
	opProduce                 // ProduceBlock
	opReorg                   // Reorg, requeue or drop
	opToggleFreeze            // SetMintFrozen on one mint
	numConvOps
)

// convOp is one drawn step. Every field is drawn for every kind so the
// sequence is plain data that two independent worlds can replay.
type convOp struct {
	Kind   int
	Mint   int
	From   int
	To     int
	Amount uint64
	Alt    bool
	Depth  uint64
	Pick   int
}

func genConvOp(t *rapid.T) convOp {
	kind := rapid.IntRange(0, numConvOps+2).Draw(t, "kind")
	if kind >= numConvOps {
		kind = opProduce // weight block production up
	}
	return convOp{
		Kind: kind,
		Mint: rapid.IntRange(0, 1).Draw(t, "mint"),
		From: rapid.IntRange(0, convOwners-1).Draw(t, "from"),
		To:   rapid.IntRange(0, convOwners-1).Draw(t, "to"),
		Amount: rapid.OneOf(
			rapid.Uint64Range(1, 400),
			rapid.Uint64Range(^uint64(0)-400, ^uint64(0)), // overflow territory
		).Draw(t, "amount"),
		Alt:   rapid.Bool().Draw(t, "alt"),
		Depth: rapid.Uint64Range(0, 5).Draw(t, "depth"),
		Pick:  rapid.IntRange(0, 1<<16).Draw(t, "pick"),
	}
}

// convWorld is one independent replay of a token-conservation sequence.
type convWorld struct {
	c      *Chain
	payer  Keypair
	owners []Keypair
	auths  [2]Keypair
	mints  [2]PublicKey
	raws   [][]byte // every transaction sent, for opResubmit
	ids    []TxID   // every TxID an accepted Submit returned
	seq    int
	final  map[TxID]TxStatus // first final status seen per TxID
	stats  map[string]int    // what this run exercised, for the non-vacuity check
}

func newConvWorld(t require.TestingT) *convWorld {
	cfg := DefaultChainConfig("sbx-prop")
	cfg.BlockhashWindow = 6
	cfg.FinalityDepth = 3
	cfg.FeePerTx = 7
	c, err := NewChain(cfg)
	require.NoError(t, err)
	w := &convWorld{c: c, payer: testKey("prop-payer"), final: map[TxID]TxStatus{}, stats: map[string]int{}}
	require.NoError(t, c.Airdrop(w.payer.PublicKey(), 1<<40))
	for i := 0; i < convOwners; i++ {
		o := testKey("prop-owner-" + strconv.Itoa(i))
		w.owners = append(w.owners, o)
		require.NoError(t, c.Airdrop(o.PublicKey(), 10_000))
	}
	var issue []Instruction
	for m := 0; m < 2; m++ {
		w.mints[m] = testAddr("prop-mint-" + strconv.Itoa(m))
		w.auths[m] = testKey("prop-auth-" + strconv.Itoa(m))
		require.NoError(t, c.CreateMint(MintConfig{Address: w.mints[m], Decimals: uint8(6 + 3*m), Authority: w.auths[m].PublicKey()}))
		for _, o := range w.owners {
			issue = append(issue, TokenMintTo{Mint: w.mints[m], ToOwner: o.PublicKey(), Amount: 1_000, Authority: w.auths[m].PublicKey()})
		}
	}
	w.send(t, []Keypair{w.auths[0], w.auths[1]}, issue...)
	c.ProduceBlock()
	return w
}

// message builds a message with the latest blockhash and a unique memo, so
// no two drawn transactions share a message.
func (w *convWorld) message(ixs ...Instruction) Message {
	w.seq++
	return Message{
		ChainID:         w.c.ID(),
		RecentBlockhash: w.c.LatestBlockhash(),
		FeePayer:        w.payer.PublicKey(),
		Instructions:    append(append([]Instruction(nil), ixs...), Memo{Data: []byte("op-" + strconv.Itoa(w.seq))}),
	}
}

// send signs and submits a valid transaction, which must be accepted.
func (w *convWorld) send(t require.TestingT, signers []Keypair, ixs ...Instruction) string {
	raw := signed(t, w.message(ixs...), append([]Keypair{w.payer}, signers...)...)
	w.raws = append(w.raws, raw)
	id, err := w.c.Submit(raw)
	require.NoError(t, err, "a correctly signed transaction on a current blockhash is accepted")
	w.ids = append(w.ids, id)
	return "submitted " + string(id)
}

func (w *convWorld) step(t require.TestingT, op convOp) string {
	mint := w.mints[op.Mint]
	from, to := w.owners[op.From], w.owners[op.To]
	switch op.Kind {
	case opTransfer:
		return w.send(t, []Keypair{from},
			TokenTransfer{Mint: mint, FromOwner: from.PublicKey(), ToOwner: to.PublicKey(), Amount: op.Amount})
	case opMintTo:
		auth := w.auths[op.Mint]
		if op.Alt {
			auth = w.auths[1-op.Mint] // fails AUTHORITY_SIGNATURE_MISSING
		}
		return w.send(t, []Keypair{auth},
			TokenMintTo{Mint: mint, ToOwner: to.PublicKey(), Amount: op.Amount, Authority: auth.PublicKey()})
	case opBurn:
		return w.send(t, []Keypair{from}, TokenBurn{Mint: mint, Owner: from.PublicKey(), Amount: op.Amount})
	case opTransferThenBurn:
		return w.send(t, []Keypair{from},
			TokenTransfer{Mint: mint, FromOwner: from.PublicKey(), ToOwner: to.PublicKey(), Amount: op.Amount},
			TokenBurn{Mint: w.mints[1-op.Mint], Owner: from.PublicKey(), Amount: op.Amount})
	case opNativeTransfer:
		return w.send(t, []Keypair{from}, SystemTransfer{From: from.PublicKey(), To: to.PublicKey(), Lamports: op.Amount})
	case opForeignSigner, opMissingSigner:
		m := w.message(TokenTransfer{Mint: mint, FromOwner: from.PublicKey(), ToOwner: to.PublicKey(), Amount: 1})
		tx, err := Sign(m, w.payer, from)
		require.NoError(t, err)
		want := CodeSignatureMissing
		if op.Kind == opForeignSigner {
			b, err := m.Bytes()
			require.NoError(t, err)
			tx.Signatures[1], err = w.owners[(op.From+1)%convOwners].Sign(b)
			require.NoError(t, err)
			want = CodeSignatureInvalid
		} else {
			tx.Signatures = tx.Signatures[:1]
		}
		_, err = w.c.Submit(encode(t, tx))
		requireReason(t, err, errs.CodeValidationFailed, want)
		return "rejected " + string(want)
	case opStaleBlockhash:
		m := w.message(TokenTransfer{Mint: mint, FromOwner: from.PublicKey(), ToOwner: to.PublicKey(), Amount: 1})
		m.RecentBlockhash = Hash{0xEE, byte(op.Pick)}
		_, err := w.c.Submit(signed(t, m, w.payer, from))
		requireReason(t, err, errs.CodeValidationFailed, CodeBlockhashNotFound)
		return "rejected stale"
	case opResubmit:
		if len(w.raws) == 0 {
			return "nothing to resubmit"
		}
		id, err := w.c.Submit(w.raws[op.Pick%len(w.raws)])
		if err == nil {
			w.ids = append(w.ids, id)
		} else {
			// Only an expired blockhash can refuse a transaction the
			// chain has already accepted once.
			requireReason(t, err, errs.CodeValidationFailed, CodeBlockhashNotFound)
		}
		return fmt.Sprintf("resubmitted %s %v", id, ReasonOf(err))
	case opProduce:
		b := w.c.ProduceBlock()
		return fmt.Sprintf("block %d %s %v", b.Height, b.Hash, b.TxIDs)
	case opReorg:
		mode := ReorgRequeue
		if op.Alt {
			mode = ReorgDrop
		}
		res, err := w.c.Reorg(op.Depth, mode)
		require.NoError(t, err)
		if len(res.TxIDs) > 0 {
			w.stats[fmt.Sprintf("reorg mode %d reverted transactions", mode)]++
		}
		return fmt.Sprintf("reorg %d %v %s", res.Removed, res.TxIDs, res.Tip.Hash)
	case opToggleFreeze:
		info, err := w.c.Mint(mint)
		require.NoError(t, err)
		require.NoError(t, w.c.SetMintFrozen(mint, !info.Frozen))
		return fmt.Sprintf("frozen %v", !info.Frozen)
	}
	t.Errorf("unknown op %d", op.Kind)
	t.FailNow()
	return ""
}

// tally records the final outcome of every distinct transaction.
func (w *convWorld) tally() {
	seen := map[TxID]bool{}
	for _, id := range w.ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		st := w.c.Status(id)
		w.stats[st.State.String()+" "+string(st.Err)]++
	}
}

// requireExercised fails when a property ran its full budget without ever
// reaching one of the outcomes it exists to test: a property that passes
// because nothing interesting happened proves nothing.
func requireExercised(t *testing.T, cases int, stats map[string]int, wanted ...string) {
	t.Helper()
	if cases < 100 {
		t.Logf("only %d cases ran; skipping the non-vacuity check", cases)
		return
	}
	for _, k := range wanted {
		require.Positive(t, stats[k], "no run reached %q; stats: %v", k, stats)
	}
	t.Logf("exercised over %d cases: %v", cases, stats)
}

// check asserts the invariants after a step.
func (w *convWorld) check(t require.TestingT) {
	require.NoError(t, w.c.audit())

	// Conservation as an outsider sees it, through the query API only.
	for _, mint := range w.mints {
		var sum uint64
		for _, o := range w.owners {
			var ok bool
			sum, ok = add64(sum, w.c.TokenBalance(o.PublicKey(), mint))
			require.True(t, ok, "token balances overflow when summed")
		}
		supply, err := w.c.Supply(mint)
		require.NoError(t, err)
		require.Equal(t, supply, sum, "mint %s: sum of balances must equal supply", mint)
	}
	native := w.c.Balance(w.payer.PublicKey()) + w.c.Balance(FeeCollectorAddress(w.c.ID()))
	for _, o := range w.owners {
		native += w.c.Balance(o.PublicKey())
	}
	require.Equal(t, w.c.NativeSupply(), native)

	// A final status never changes again.
	for _, id := range w.ids {
		st := w.c.Status(id)
		if prev, seen := w.final[id]; seen {
			require.Equal(t, prev, st, "final status of %s changed", id)
		} else if st.Final {
			w.final[id] = st
		}
	}
}

// TestProp_TokenConservation: across random valid and invalid transaction
// sequences interleaved with block production, reorgs and freezes, every
// mint's balances sum to its supply after every step, native supply is
// conserved, no balance underflows (the audit sees uint64 sums agree), no
// TxID sits in two canonical block positions, final statuses never change,
// and two independent replays of the same sequence agree on every outcome
// and every StateHash.
func TestProp_TokenConservation(t *testing.T) {
	stats := map[string]int{}
	cases := 0
	rapid.Check(t, func(t *rapid.T) {
		cases++
		// rapid favours short slices; at least 10 steps make every case a
		// real history of submissions, blocks and reorgs.
		ops := rapid.SliceOfN(rapid.Custom(genConvOp), 10, 80).Draw(t, "ops")
		a, b := newConvWorld(t), newConvWorld(t)
		require.Equal(t, a.c.StateHash(), b.c.StateHash())
		for i, op := range ops {
			outA, outB := a.step(t, op), b.step(t, op)
			require.Equal(t, outA, outB, "step %d (%+v) diverged between replays", i, op)
			a.check(t)
			require.NoError(t, b.c.audit())
			require.Equal(t, a.c.StateHash(), b.c.StateHash(), "step %d: state hashes diverged", i)
		}
		// Settle: drain, finalize, and check once more.
		a.c.ClearFaults()
		a.c.ProduceBlocks(6 + 3 + 1)
		b.c.ProduceBlocks(6 + 3 + 1)
		require.Zero(t, a.c.PendingCount())
		a.check(t)
		require.Equal(t, a.c.StateHash(), b.c.StateHash())
		require.Equal(t, a.c.Tip(), b.c.Tip())
		a.tally()
		for k, v := range a.stats {
			stats[k] += v
		}
	})
	requireExercised(t, cases, stats,
		"FINALIZED ",
		"FAILED "+string(CodeInsufficientTokenBalance),
		"FAILED "+string(CodeInsufficientFunds),
		"FAILED "+string(CodeAuthoritySignatureMissing),
		"FAILED "+string(CodeArithmeticOverflow),
		"FAILED "+string(CodeMintFrozen),
		"NOT_FOUND ", // forgotten by a ReorgDrop or by expiry after a requeue
		fmt.Sprintf("reorg mode %d reverted transactions", ReorgRequeue),
		fmt.Sprintf("reorg mode %d reverted transactions", ReorgDrop),
	)
}

// ---------------------------------------------------------------------------
// TestProp_BridgeNoDoubleCount
// ---------------------------------------------------------------------------

const (
	propSrc    ChainID = "sbx-src"
	propDst    ChainID = "sbx-dst"
	propOwners         = 3
	propWindow         = 8
	propDepth          = 3
)

// Bridge operations. Random sequences rarely walk lock -> finalize -> relay
// -> relay again on their own, so production makes several blocks at once,
// relays usually pick a lock that is already final, and one operation
// targets an already relayed lock; requireExercised proves each path is hit.
const (
	bOpLock           = iota // a lock with a fresh nonce (may exceed the balance)
	bOpLockReuse             // a lock reusing an earlier nonce
	bOpProduceA              // 1 to 5 blocks on the source
	bOpProduceB              // 1 to 5 blocks on the destination
	bOpRelay                 // relay a lock: a final one when Alt and any exist, else any lock
	bOpRelayDuplicate        // relay an already relayed lock again, from the second relayer
	bOpRelayResend           // resend an earlier relay's exact bytes
	bOpReorgA                // Reorg on the source
	bOpReorgB                // Reorg on the destination
	numBridgeOps
)

type bridgeOp struct {
	Kind   int
	Owner  int
	Dest   int
	Amount uint64
	Pick   int
	Depth  uint64
	Alt    bool
}

func genBridgeOp(t *rapid.T) bridgeOp {
	kind := rapid.IntRange(0, numBridgeOps+3).Draw(t, "kind")
	switch kind {
	case numBridgeOps, numBridgeOps + 1:
		kind = bOpRelay
	case numBridgeOps + 2:
		kind = bOpProduceA
	case numBridgeOps + 3:
		kind = bOpRelayDuplicate
	}
	return bridgeOp{
		Kind:   kind,
		Owner:  rapid.IntRange(0, propOwners-1).Draw(t, "owner"),
		Dest:   rapid.IntRange(0, propOwners-1).Draw(t, "dest"),
		Amount: rapid.Uint64Range(1, 600).Draw(t, "amount"), // owners start with 500
		Pick:   rapid.IntRange(0, 1<<16).Draw(t, "pick"),
		Depth:  rapid.Uint64Range(0, propDepth+1).Draw(t, "depth"),
		Alt:    rapid.Bool().Draw(t, "alt"),
	}
}

type propBridge struct {
	net           *Network
	a, b          *Chain
	attester      Keypair
	payer         Keypair // fee payer on the source
	relayers      [2]Keypair
	owners, dests []Keypair
	issuer        Keypair
	usdA, usdB    PublicKey
	nextNonce     uint64
	locks         []TxID
	nonces        []uint64
	relays        [][]byte
	relayed       []TxID // locks whose release was submitted
	stats         map[string]int
}

func newPropBridge(t require.TestingT) *propBridge {
	p := &propBridge{
		net: NewNetwork(), attester: testKey("prop-attester"), payer: testKey("prop-src-payer"),
		relayers: [2]Keypair{testKey("prop-relayer-0"), testKey("prop-relayer-1")},
		issuer:   testKey("prop-issuer"), usdA: testAddr("prop-usd-a"), usdB: testAddr("prop-usd-b"),
		nextNonce: 1, stats: map[string]int{},
	}
	cfg := func(id ChainID) ChainConfig {
		c := DefaultChainConfig(id)
		c.BlockhashWindow, c.FinalityDepth, c.FeePerTx = propWindow, propDepth, 3
		c.Bridge = &BridgeConfig{Authority: p.attester.PublicKey()}
		return c
	}
	var err error
	p.a, err = p.net.AddChain(cfg(propSrc))
	require.NoError(t, err)
	p.b, err = p.net.AddChain(cfg(propDst))
	require.NoError(t, err)
	require.NoError(t, p.a.Airdrop(p.payer.PublicKey(), 1<<40))
	for _, r := range p.relayers {
		require.NoError(t, p.b.Airdrop(r.PublicKey(), 1<<40))
	}
	require.NoError(t, p.a.CreateMint(MintConfig{Address: p.usdA, Decimals: 6, Authority: p.issuer.PublicKey()}))
	require.NoError(t, p.b.CreateMint(MintConfig{Address: p.usdB, Decimals: 6, Authority: testKey("prop-issuer-b").PublicKey()}))
	var issue []Instruction
	for i := 0; i < propOwners; i++ {
		p.owners = append(p.owners, testKey("prop-src-owner-"+strconv.Itoa(i)))
		p.dests = append(p.dests, testKey("prop-dst-owner-"+strconv.Itoa(i)))
		issue = append(issue, TokenMintTo{Mint: p.usdA, ToOwner: p.owners[i].PublicKey(), Amount: 500, Authority: p.issuer.PublicKey()})
	}
	m := Message{ChainID: propSrc, RecentBlockhash: p.a.LatestBlockhash(), FeePayer: p.payer.PublicKey(), Instructions: issue}
	_, err = p.a.Submit(signed(t, m, p.payer, p.issuer))
	require.NoError(t, err)
	p.a.ProduceBlocks(1 + propDepth)
	return p
}

func (p *propBridge) submitLock(t require.TestingT, owner, dest int, amount, nonce uint64) {
	m := Message{
		ChainID: propSrc, RecentBlockhash: p.a.LatestBlockhash(), FeePayer: p.payer.PublicKey(),
		Instructions: []Instruction{BridgeLock{
			Mint: p.usdA, Owner: p.owners[owner].PublicKey(), Amount: amount,
			DestChain: propDst, DestOwner: p.dests[dest].PublicKey(), Nonce: nonce,
		}},
	}
	id, err := p.a.Submit(signed(t, m, p.payer, p.owners[owner]))
	require.NoError(t, err)
	p.locks = append(p.locks, id)
	p.nonces = append(p.nonces, nonce)
}

// relay builds and submits a release for lockID if, and only if, the lock
// is FINALIZED; BuildRelease must refuse anything else.
func (p *propBridge) relay(t require.TestingT, lockID TxID, relayer Keypair) string {
	lt, err := p.a.LockedTransfer(lockID)
	if err != nil {
		// A lock forgotten by a ReorgDrop or expiry is simply not there.
		requireReason(t, err, errs.CodeNotFound, CodeTransactionNotFound)
		return "lock gone"
	}
	m, err := BuildRelease(lt, ReleaseParams{
		DestMint: p.usdB, Authority: p.attester.PublicKey(),
		FeePayer: relayer.PublicKey(), RecentBlockhash: p.b.LatestBlockhash(),
	})
	if lt.Status.State != TxFinalized {
		requireReason(t, err, errs.CodeInvalidStateTransition, CodeLockNotFinalized)
		p.stats["relay refused for a lock that is "+lt.Status.State.String()]++
		return "refused " + lt.Status.State.String()
	}
	require.NoError(t, err)
	signers := []Keypair{relayer, p.attester}
	raw := signed(t, m, signers...)
	p.relays = append(p.relays, raw)
	id, err := p.b.Submit(raw)
	require.NoError(t, err) // a repeat of an identical message is idempotent
	p.relayed = append(p.relayed, lockID)
	return "relayed " + string(id)
}

// finalLocks returns the submitted locks that are FINALIZED now.
func (p *propBridge) finalLocks() []TxID {
	var out []TxID
	for _, id := range p.locks {
		if p.a.Status(id).State == TxFinalized {
			out = append(out, id)
		}
	}
	return out
}

func (p *propBridge) step(t require.TestingT, op bridgeOp) string {
	pick := func(n int) int { return op.Pick % n }
	mode := ReorgRequeue
	if op.Alt {
		mode = ReorgDrop
	}
	switch op.Kind {
	case bOpLock:
		p.submitLock(t, op.Owner, op.Dest, op.Amount, p.nextNonce)
		p.nextNonce++
		return "lock"
	case bOpLockReuse:
		if len(p.nonces) == 0 {
			return "no nonce to reuse"
		}
		p.submitLock(t, op.Owner, op.Dest, op.Amount, p.nonces[pick(len(p.nonces))])
		return "lock reusing a nonce"
	case bOpProduceA:
		return "A " + p.a.ProduceBlocks(1+op.Depth).Hash.String()
	case bOpProduceB:
		return "B " + p.b.ProduceBlocks(1+op.Depth).Hash.String()
	case bOpRelay:
		candidates := p.locks
		if final := p.finalLocks(); op.Alt && len(final) > 0 {
			candidates = final
		}
		if len(candidates) == 0 {
			return "nothing to relay"
		}
		return p.relay(t, candidates[pick(len(candidates))], p.relayers[0])
	case bOpRelayDuplicate:
		if len(p.relayed) == 0 {
			return "nothing relayed yet"
		}
		return p.relay(t, p.relayed[pick(len(p.relayed))], p.relayers[1])
	case bOpRelayResend:
		if len(p.relays) == 0 {
			return "nothing to resend"
		}
		_, err := p.b.Submit(p.relays[pick(len(p.relays))])
		if err != nil {
			requireReason(t, err, errs.CodeValidationFailed, CodeBlockhashNotFound)
		}
		return "resent"
	case bOpReorgA:
		res, err := p.a.Reorg(op.Depth, mode)
		require.NoError(t, err)
		if len(res.TxIDs) > 0 {
			p.stats["source reorg reverted transactions"]++
		}
		return fmt.Sprintf("reorg A %d %v", res.Removed, res.TxIDs)
	case bOpReorgB:
		res, err := p.b.Reorg(op.Depth, mode)
		require.NoError(t, err)
		if len(res.TxIDs) > 0 {
			p.stats["destination reorg reverted transactions"]++
		}
		return fmt.Sprintf("reorg B %d %v", res.Removed, res.TxIDs)
	}
	t.Errorf("unknown op %d", op.Kind)
	t.FailNow()
	return ""
}

// canonicalSuccesses returns every instruction of type T in successful
// canonical transactions of c, with the transaction's status.
func canonicalSuccesses[T Instruction](t require.TestingT, c *Chain) (out []T, ids []TxID, statuses []TxStatus) {
	tip := c.Tip().Height
	for h := uint64(1); h <= tip; h++ {
		blk, err := c.Block(h)
		require.NoError(t, err)
		for _, id := range blk.TxIDs {
			rec, err := c.GetTransaction(id)
			require.NoError(t, err)
			if rec.Status.State == TxFailed {
				continue
			}
			for _, ins := range rec.Transaction.Message.Instructions {
				if v, ok := ins.(T); ok {
					out, ids, statuses = append(out, v), append(ids, id), append(statuses, rec.Status)
				}
			}
		}
	}
	return out, ids, statuses
}

// check asserts the no-double-count invariants after a step.
func (p *propBridge) check(t require.TestingT) {
	require.NoError(t, p.a.audit())
	require.NoError(t, p.b.audit())

	locks, _, lockStatuses := canonicalSuccesses[BridgeLock](t, p.a)
	var locked, finalizedLocked uint64
	for i, l := range locks {
		locked += l.Amount
		if lockStatuses[i].State == TxFinalized {
			finalizedLocked += l.Amount
		}
	}
	require.Equal(t, locked, p.a.TokenBalance(BridgeEscrowAddress(propSrc), p.usdA),
		"the source escrow holds exactly the canonically locked amount")

	releases, _, _ := canonicalSuccesses[BridgeRelease](t, p.b)
	bySource := map[TxID]int{}
	byNonce := map[uint64]int{}
	var released uint64
	for _, r := range releases {
		bySource[r.SourceTxID]++
		byNonce[r.Nonce]++
		require.LessOrEqual(t, bySource[r.SourceTxID], 1, "lock %s released twice", r.SourceTxID)
		require.LessOrEqual(t, byNonce[r.Nonce], 1, "nonce %d released twice", r.Nonce)
		released += r.Amount

		// Every release stands on a FINALIZED, successful lock with the same
		// economics: a reorg on the source can never strand it.
		lt, err := p.a.LockedTransfer(r.SourceTxID)
		require.NoError(t, err)
		require.Equal(t, TxFinalized, lt.Status.State)
		require.Equal(t, r.Amount, lt.Lock.Amount)
		require.Equal(t, r.ToOwner, lt.Lock.DestOwner)
		require.Equal(t, r.Nonce, lt.Lock.Nonce)
		require.Equal(t, propSrc, r.SourceChain)
	}
	supplyB, err := p.b.Supply(p.usdB)
	require.NoError(t, err)
	require.Equal(t, released, supplyB, "the destination mints only through releases")
	require.LessOrEqual(t, released, finalizedLocked, "released on the destination never exceeds finalized locks on the source")
}

// settle drains both chains, relays every finalized lock that has no
// release, and requires the two sides to balance exactly.
func (p *propBridge) settle(t require.TestingT) {
	drain := func(c *Chain) {
		for i := 0; c.PendingCount() > 0; i++ {
			require.Less(t, i, 4*propWindow, "mempool does not drain")
			c.ProduceBlock()
		}
		c.ProduceBlocks(propDepth + 1)
	}
	drain(p.a)
	locks, lockIDs, _ := canonicalSuccesses[BridgeLock](t, p.a)
	releases, _, _ := canonicalSuccesses[BridgeRelease](t, p.b)
	done := map[TxID]bool{}
	for _, r := range releases {
		done[r.SourceTxID] = true
	}
	for _, id := range lockIDs {
		if !done[id] {
			require.Contains(t, p.relay(t, id, p.relayers[0]), "relayed")
		}
	}
	drain(p.b)
	p.check(t)

	releases, _, releaseStatuses := canonicalSuccesses[BridgeRelease](t, p.b)
	require.Len(t, releases, len(locks), "every finalized lock is released exactly once")
	var locked, released uint64
	for _, l := range locks {
		locked += l.Amount
	}
	for i, r := range releases {
		require.Equal(t, TxFinalized, releaseStatuses[i].State)
		released += r.Amount
	}
	require.Equal(t, locked, released, "finalized locks and finalized releases balance")
	require.Equal(t, locked, p.a.TokenBalance(BridgeEscrowAddress(propSrc), p.usdA))
}

// tally records the canonical outcomes on both chains after settle.
func (p *propBridge) tally(t require.TestingT) {
	for _, side := range []struct {
		name string
		c    *Chain
	}{{"source", p.a}, {"destination", p.b}} {
		tip := side.c.Tip().Height
		for h := uint64(1); h <= tip; h++ {
			blk, err := side.c.Block(h)
			require.NoError(t, err)
			for _, id := range blk.TxIDs {
				st := side.c.Status(id)
				p.stats[side.name+" "+st.State.String()+" "+string(st.Err)]++
			}
		}
	}
}

// TestProp_BridgeNoDoubleCount: across random lock, relay, duplicate relay
// and reorg sequences on both chains, every release on the destination
// stands on a finalized lock with the same amount, owner and nonce; no lock
// or nonce is released twice; the destination's supply equals what was
// released and never exceeds the finalized locked amount; and once both
// chains settle, finalized locks and finalized releases balance exactly.
//
// A second world replays every sequence in lockstep: outcomes and the
// Network StateHash, which commits to escrow, lock nonces and releases, must
// agree after every step.
func TestProp_BridgeNoDoubleCount(t *testing.T) {
	stats := map[string]int{}
	cases := 0
	rapid.Check(t, func(t *rapid.T) {
		cases++
		p, q := newPropBridge(t), newPropBridge(t)
		// rapid favours short slices (about six elements on average); a
		// bridge history needs dozens of steps to lock, finalize, relay and
		// relay again, so every sequence is at least 25 long.
		ops := rapid.SliceOfN(rapid.Custom(genBridgeOp), 25, 100).Draw(t, "ops")
		for i, op := range ops {
			outP, outQ := p.step(t, op), q.step(t, op)
			require.Equal(t, outP, outQ, "step %d (%+v) diverged between replays", i, op)
			p.check(t)
			require.Equal(t, p.net.StateHash(), q.net.StateHash(), "step %d: state hashes diverged", i)
		}
		p.settle(t)
		q.settle(t)
		require.Equal(t, p.net.StateHash(), q.net.StateHash())
		p.tally(t)
		for k, v := range p.stats {
			stats[k] += v
		}
	})
	requireExercised(t, cases, stats,
		"destination FINALIZED ",
		"destination FAILED "+string(CodeBridgeAlreadyReleased),
		"source FINALIZED ",
		"source FAILED "+string(CodeBridgeNonceUsed),
		"source FAILED "+string(CodeInsufficientTokenBalance),
		"relay refused for a lock that is CONFIRMED",
		"relay refused for a lock that is PENDING",
		"source reorg reverted transactions",
		"destination reorg reverted transactions",
	)
}
