package devnet

// execute runs one transaction against the chain state, Solana-like: the fee
// is charged first and stays charged; the instructions then run in one
// overlay that commits only if every one of them succeeds. It returns the fee
// charged, the failure ("" on success) and the forward effects to journal.
//
// A fee payer who cannot pay the fee fails the transaction with
// INSUFFICIENT_FUNDS_FOR_FEE and nothing moves at all.
func (c *Chain) execute(rec *txRecord) (fee uint64, failure ErrorCode, effects []effect) {
	msg := rec.tx.Message
	if c.cfg.FeePerTx > 0 {
		t := c.state.begin()
		if code := t.debitNative(msg.FeePayer, c.cfg.FeePerTx); code != "" {
			return 0, CodeInsufficientFundsForFee, nil
		}
		// The collector's balance is part of native supply, which fits a
		// uint64, so this cannot overflow; if it did, the overlay is
		// discarded and nothing moves.
		if code := t.creditNative(c.cfg.FeeCollector, c.cfg.FeePerTx); code != "" {
			return 0, code, nil
		}
		effects = t.commit()
		fee = c.cfg.FeePerTx
	}
	body := c.state.begin()
	for _, ins := range msg.Instructions {
		if code := c.apply(body, ins, rec.id); code != "" {
			return fee, code, effects // body is discarded: only the fee moved
		}
	}
	return fee, "", append(effects, body.commit()...)
}

// apply executes one instruction inside t. Signatures were verified at
// Submit; what is checked here is everything that depends on state.
func (c *Chain) apply(t *txn, raw Instruction, tx Signature) ErrorCode {
	ins, ok := canonical(raw)
	if !ok {
		return CodeInvalidMessage
	}
	switch in := ins.(type) {
	case SystemTransfer:
		if code := t.debitNative(in.From, in.Lamports); code != "" {
			return code
		}
		return t.creditNative(in.To, in.Lamports)

	case TokenTransfer:
		if code := c.liveMint(in.Mint); code != "" {
			return code
		}
		if code := t.debitToken(in.Mint, in.FromOwner, in.Amount); code != "" {
			return code
		}
		return t.creditToken(in.Mint, in.ToOwner, in.Amount)

	case TokenMintTo:
		m, exists := c.state.mints[in.Mint]
		switch {
		case !exists:
			return CodeMintNotFound
		case m.authority != in.Authority:
			return CodeAuthoritySignatureMissing
		case m.frozen:
			return CodeMintFrozen
		}
		if code := t.increaseSupply(in.Mint, in.Amount); code != "" {
			return code
		}
		return t.creditToken(in.Mint, in.ToOwner, in.Amount)

	case TokenBurn:
		if code := c.liveMint(in.Mint); code != "" {
			return code
		}
		if code := t.debitToken(in.Mint, in.Owner, in.Amount); code != "" {
			return code
		}
		return t.decreaseSupply(in.Mint, in.Amount)

	case Memo:
		return "" // recorded in the transaction; changes no state

	case BridgeLock:
		if c.cfg.Bridge == nil {
			return CodeBridgeNotConfigured
		}
		if code := c.liveMint(in.Mint); code != "" {
			return code
		}
		if code := t.useLockNonce(in.Nonce, tx); code != "" {
			return code
		}
		if code := t.debitToken(in.Mint, in.Owner, in.Amount); code != "" {
			return code
		}
		return t.creditToken(in.Mint, BridgeEscrowAddress(c.cfg.ID), in.Amount)

	case BridgeRelease:
		if c.cfg.Bridge == nil {
			return CodeBridgeNotConfigured
		}
		if in.Authority != c.cfg.Bridge.Authority {
			return CodeAuthoritySignatureMissing
		}
		src, err := in.SourceTxID.Signature()
		if err != nil {
			return CodeInvalidMessage // unreachable: Validate parsed it at Submit
		}
		if code := t.consumeRelease(in.SourceChain, src, in.Nonce, tx); code != "" {
			return code
		}
		if code := c.liveMint(in.Mint); code != "" {
			return code
		}
		if code := t.increaseSupply(in.Mint, in.Amount); code != "" {
			return code
		}
		return t.creditToken(in.Mint, in.ToOwner, in.Amount)
	}
	return CodeInvalidMessage // unreachable: the instruction set is closed
}

// liveMint checks that mint exists and is not frozen.
func (c *Chain) liveMint(mint PublicKey) ErrorCode {
	m, ok := c.state.mints[mint]
	switch {
	case !ok:
		return CodeMintNotFound
	case m.frozen:
		return CodeMintFrozen
	}
	return ""
}
