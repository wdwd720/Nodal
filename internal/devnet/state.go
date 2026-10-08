package devnet

// This file holds a chain's balances, supplies and bridge bookkeeping, and
// the copy-on-write overlay (txn) through which every change is made. A txn
// either commits as a whole or is discarded, which is what makes a
// transaction atomic and a reorg all-or-nothing.

// tokenKey addresses one owner's balance of one mint.
type tokenKey struct {
	mint  PublicKey
	owner PublicKey
}

// releaseKey is the (source chain, source tx id) a release consumes.
type releaseKey struct {
	source   ChainID
	sourceTx Signature
}

// releaseNonceKey is the (source chain, nonce) a release consumes.
type releaseNonceKey struct {
	source ChainID
	nonce  uint64
}

// releaseRecord is what a consumed releaseKey remembers.
type releaseRecord struct {
	nonce uint64
	tx    Signature // the BRIDGE_RELEASE transaction that consumed it
}

// mintInfo is a mint's configuration. Supply lives in ledger.supply so that
// it changes through the same overlay as balances.
type mintInfo struct {
	decimals  uint8
	authority PublicKey
	frozen    bool
}

// ledger is the state of one chain. Balance maps never store zero, so two
// histories that end with the same balances hold identical maps.
type ledger struct {
	native        map[PublicKey]uint64
	nativeSupply  uint64
	mints         map[PublicKey]mintInfo
	supply        map[PublicKey]uint64
	tokens        map[tokenKey]uint64
	lockNonces    map[uint64]Signature // BRIDGE_LOCK nonce -> lock transaction
	releases      map[releaseKey]releaseRecord
	releaseNonces map[releaseNonceKey]Signature // -> release transaction
}

func newLedger() ledger {
	return ledger{
		native:        map[PublicKey]uint64{},
		mints:         map[PublicKey]mintInfo{},
		supply:        map[PublicKey]uint64{},
		tokens:        map[tokenKey]uint64{},
		lockNonces:    map[uint64]Signature{},
		releases:      map[releaseKey]releaseRecord{},
		releaseNonces: map[releaseNonceKey]Signature{},
	}
}

// layer is a copy-on-write overlay over one base map. Reads fall through to
// base; writes and deletions stay in the overlay until commit. A key is in at
// most one of written and deleted.
type layer[K comparable, V any] struct {
	base    map[K]V
	written map[K]V
	deleted map[K]struct{}
}

func (l *layer[K, V]) get(k K) (V, bool) {
	if v, ok := l.written[k]; ok {
		return v, true
	}
	if _, gone := l.deleted[k]; gone {
		var zero V
		return zero, false
	}
	v, ok := l.base[k]
	return v, ok
}

func (l *layer[K, V]) put(k K, v V) {
	if l.written == nil {
		l.written = map[K]V{}
	}
	l.written[k] = v
	delete(l.deleted, k)
}

func (l *layer[K, V]) remove(k K) {
	delete(l.written, k)
	if l.deleted == nil {
		l.deleted = map[K]struct{}{}
	}
	l.deleted[k] = struct{}{}
}

// commit applies the overlay to base. The result does not depend on map
// iteration order: deletions and writes touch disjoint keys.
func (l *layer[K, V]) commit() {
	for k := range l.deleted {
		delete(l.base, k)
	}
	for k, v := range l.written {
		l.base[k] = v
	}
}

// effectKind enumerates the forward changes a block records so a reorg can
// undo them.
type effectKind uint8

const (
	effNativeCredit effectKind = iota + 1
	effNativeDebit
	effTokenCredit
	effTokenDebit
	effSupplyIncrease
	effSupplyDecrease
	effLockNonce
	effRelease
)

// effect is one forward change. Balance and supply effects are deltas, not
// before/after values, so undoing them commutes with operator airdrops made
// after the block.
type effect struct {
	kind   effectKind
	key    PublicKey // account (native), or mint (token, supply)
	owner  PublicKey // token owner
	amount uint64
	nonce  uint64
	chain  ChainID   // release source chain
	ref    Signature // release source tx
	tx     Signature // the lock or release transaction (set effects)
}

// txn is an atomic unit of change over a ledger: one transaction's fee, one
// transaction's instructions, or one reorg's reversal. Nothing reaches the
// ledger until commit.
type txn struct {
	native        layer[PublicKey, uint64]
	supply        layer[PublicKey, uint64]
	tokens        layer[tokenKey, uint64]
	lockNonces    layer[uint64, Signature]
	releases      layer[releaseKey, releaseRecord]
	releaseNonces layer[releaseNonceKey, Signature]
	effects       []effect
}

func (l *ledger) begin() *txn {
	return &txn{
		native:        layer[PublicKey, uint64]{base: l.native},
		supply:        layer[PublicKey, uint64]{base: l.supply},
		tokens:        layer[tokenKey, uint64]{base: l.tokens},
		lockNonces:    layer[uint64, Signature]{base: l.lockNonces},
		releases:      layer[releaseKey, releaseRecord]{base: l.releases},
		releaseNonces: layer[releaseNonceKey, Signature]{base: l.releaseNonces},
	}
}

// commit applies every change and returns the forward effects in order.
func (t *txn) commit() []effect {
	t.native.commit()
	t.supply.commit()
	t.tokens.commit()
	t.lockNonces.commit()
	t.releases.commit()
	t.releaseNonces.commit()
	return t.effects
}

func (t *txn) record(e effect) { t.effects = append(t.effects, e) }

// setBalance stores v, or deletes the key for zero so maps stay sparse.
func setBalance[K comparable](l *layer[K, uint64], k K, v uint64) {
	if v == 0 {
		l.remove(k)
		return
	}
	l.put(k, v)
}

// credit adds amt to l[k]; failure is ARITHMETIC_OVERFLOW.
func credit[K comparable](l *layer[K, uint64], k K, amt uint64) ErrorCode {
	cur, _ := l.get(k)
	next, ok := add64(cur, amt)
	if !ok {
		return CodeArithmeticOverflow
	}
	setBalance(l, k, next)
	return ""
}

// debit subtracts amt from l[k]; failure is the given code.
func debit[K comparable](l *layer[K, uint64], k K, amt uint64, short ErrorCode) ErrorCode {
	cur, _ := l.get(k)
	if cur < amt {
		return short
	}
	setBalance(l, k, cur-amt)
	return ""
}

func (t *txn) creditNative(k PublicKey, amt uint64) ErrorCode {
	if code := credit(&t.native, k, amt); code != "" {
		return code
	}
	t.record(effect{kind: effNativeCredit, key: k, amount: amt})
	return ""
}

func (t *txn) debitNative(k PublicKey, amt uint64) ErrorCode {
	if code := debit(&t.native, k, amt, CodeInsufficientFunds); code != "" {
		return code
	}
	t.record(effect{kind: effNativeDebit, key: k, amount: amt})
	return ""
}

func (t *txn) creditToken(mint, owner PublicKey, amt uint64) ErrorCode {
	if code := credit(&t.tokens, tokenKey{mint, owner}, amt); code != "" {
		return code
	}
	t.record(effect{kind: effTokenCredit, key: mint, owner: owner, amount: amt})
	return ""
}

func (t *txn) debitToken(mint, owner PublicKey, amt uint64) ErrorCode {
	if code := debit(&t.tokens, tokenKey{mint, owner}, amt, CodeInsufficientTokenBalance); code != "" {
		return code
	}
	t.record(effect{kind: effTokenDebit, key: mint, owner: owner, amount: amt})
	return ""
}

func (t *txn) increaseSupply(mint PublicKey, amt uint64) ErrorCode {
	if code := credit(&t.supply, mint, amt); code != "" {
		return code
	}
	t.record(effect{kind: effSupplyIncrease, key: mint, amount: amt})
	return ""
}

func (t *txn) decreaseSupply(mint PublicKey, amt uint64) ErrorCode {
	// Burns debit a balance first and every balance is part of supply, so a
	// shortfall here means the books are already wrong.
	if code := debit(&t.supply, mint, amt, CodeInvariantViolated); code != "" {
		return code
	}
	t.record(effect{kind: effSupplyDecrease, key: mint, amount: amt})
	return ""
}

// useLockNonce consumes a BRIDGE_LOCK nonce for lockTx.
func (t *txn) useLockNonce(nonce uint64, lockTx Signature) ErrorCode {
	if _, used := t.lockNonces.get(nonce); used {
		return CodeBridgeNonceUsed
	}
	t.lockNonces.put(nonce, lockTx)
	t.record(effect{kind: effLockNonce, nonce: nonce, tx: lockTx})
	return ""
}

// consumeRelease consumes both (source, sourceTx) and (source, nonce) for
// releaseTx. Either one already consumed is BRIDGE_ALREADY_RELEASED.
func (t *txn) consumeRelease(source ChainID, sourceTx Signature, nonce uint64, releaseTx Signature) ErrorCode {
	rk := releaseKey{source: source, sourceTx: sourceTx}
	nk := releaseNonceKey{source: source, nonce: nonce}
	if _, used := t.releases.get(rk); used {
		return CodeBridgeAlreadyReleased
	}
	if _, used := t.releaseNonces.get(nk); used {
		return CodeBridgeAlreadyReleased
	}
	t.releases.put(rk, releaseRecord{nonce: nonce, tx: releaseTx})
	t.releaseNonces.put(nk, releaseTx)
	t.record(effect{kind: effRelease, chain: source, ref: sourceTx, nonce: nonce, tx: releaseTx})
	return ""
}

// revert applies the inverse of a forward effect. It checks every step: a
// failure means the books disagree with the journal, and the caller must
// discard the whole txn.
func (t *txn) revert(e effect) ErrorCode {
	switch e.kind {
	case effNativeCredit:
		return debit(&t.native, e.key, e.amount, CodeInvariantViolated)
	case effNativeDebit:
		return credit(&t.native, e.key, e.amount)
	case effTokenCredit:
		return debit(&t.tokens, tokenKey{e.key, e.owner}, e.amount, CodeInvariantViolated)
	case effTokenDebit:
		return credit(&t.tokens, tokenKey{e.key, e.owner}, e.amount)
	case effSupplyIncrease:
		return debit(&t.supply, e.key, e.amount, CodeInvariantViolated)
	case effSupplyDecrease:
		return credit(&t.supply, e.key, e.amount)
	case effLockNonce:
		if got, ok := t.lockNonces.get(e.nonce); !ok || got != e.tx {
			return CodeInvariantViolated
		}
		t.lockNonces.remove(e.nonce)
		return ""
	case effRelease:
		rk := releaseKey{source: e.chain, sourceTx: e.ref}
		nk := releaseNonceKey{source: e.chain, nonce: e.nonce}
		rec, ok := t.releases.get(rk)
		byNonce, okNonce := t.releaseNonces.get(nk)
		if !ok || !okNonce || rec.tx != e.tx || byNonce != e.tx {
			return CodeInvariantViolated
		}
		t.releases.remove(rk)
		t.releaseNonces.remove(nk)
		return ""
	default:
		return CodeInvariantViolated
	}
}
