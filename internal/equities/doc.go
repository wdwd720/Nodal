// Package equities is the equities and ETFs subsystem of the advanced
// financial capabilities, in PAPER and SANDBOX mode only (ADR-0020,
// docs/architecture/ADVANCED_FINANCIAL_MODEL.md, capability EQUITIES,
// migration 00743).
//
// An equity or ETF is a first-class instrument (instruments.CreateAdvanced,
// kind EQUITY or ETF) whose shares are one SIMULATED asset per listing, with
// decimals equal to the fractional-share precision. Shares are held in the
// customer's WALLET like any other asset, so capital reservations, buying
// power and the ledger's non-negative balances apply to them unchanged.
//
// # Responsibilities
//
//   - Calendar: the XNYS-style US equity calendar in America/New_York
//     (pre-market 04:00-09:30, regular 09:30-16:00, after-hours 16:00-20:00,
//     weekends and an injectable list of full-day holidays closed), trade
//     dates, T+1 settlement dates and the ex-date rule for dividends.
//
//   - The broker boundary (EquitiesExecutionAdapter): a deterministic paper
//     venue that fills market orders at the provided mark plus a disclosed
//     slippage and limit orders only when the mark crosses the limit, a
//     fixture-replay adapter driven by a recorded conversation in testdata,
//     and provider-mode handling: fake and sandbox only in LOCAL, TEST and
//     DEV; live refused unconditionally because it does not exist.
//
//   - The order lifecycle, one database transaction per step. The venue call
//     is external, so the order is recorded before it is sent and the
//     venue's answer is applied after, in a transaction of its own; each step
//     moves the order and its economic intent by at most one status, which
//     also keeps it within the one-change-per-transaction binding of 00726.
//
//     SubmitOrderIntent: econ intent RECEIVED, routed -> ROUTED (or REJECTED).
//     AcceptOrder: authorize, validate, price, risk, reserve; the order is
//     ACCEPTED (or REJECTED) and the intent RISK_APPROVED (or REJECTED). An
//     intent recorded before a split or reverse split was applied is refused:
//     its quantity and limit are in units the instrument no longer trades in.
//     PlaceAtVenue / PollVenue / CancelAtVenue: the venue call, outside any
//     transaction; it writes nothing. Only a PENDING submission is placed.
//     ApplyOrderReport: OPEN, FILLED (trade posted, settlement scheduled),
//     REJECTED, CANCELLED or EXPIRED; the intent becomes EXECUTING (or ends).
//     SettleDueBatch: EQUITY_SETTLEMENT on the settlement date,
//     one account per transaction, each settlement in its own savepoint; the
//     intent COMPLETED. A settlement that cannot settle is rolled back alone,
//     backed off (upkeep_failures, 00760) and reported.
//
//   - Risk: Risk Kernel V2 with every section it requires. An agent's order
//     needs the authority section and a strategy-originated order the
//     strategy section; equities keeps neither register, so the composition
//     root wires an AuthorityReader and a StrategyReader, and without them
//     such orders are refused (the section is missing).
//
//   - Trade-date accounting (SIM_TRADE): a buyer pays at trade date and the
//     shares wait in UNSETTLED_RECEIVABLE until settlement; a seller's shares
//     leave at trade date and the proceeds, net of the disclosed spread and
//     fees, wait in UNSETTLED_RECEIVABLE. The paper venue is external and is
//     never booked. Spread and fees come from internal/fees: the full
//     disclosure and the schedule that produced it are stored with the fill,
//     and fees.VerifyQuote is re-run before anything is posted.
//
//   - Fee booking: every spread and fee a fill charges is booked through the
//     fee ledger (internal/feeledger) in the transaction that applies the
//     fill -- the schedule ensured in force under its equities key, the
//     payer's disclosure byte for byte the fill's own, every component on its
//     own pair inside the fill's journal and registered against it and its
//     effect (feebooking.go). The database refuses a fill whose charges the
//     fee ledger did not book (00764, FW001).
//
//   - Corporate actions: split, reverse split (cash-in-lieu for fractional
//     remainders under an explicit rounding policy), cash dividend for
//     holders of record, symbol change (a terms amendment, no posting) and
//     delisting (instrument DELISTING, then DELISTED). One live action per
//     instrument, type and effective date; a cancelled action frees its date.
//
// # Market orders outside regular hours
//
// They are refused, never queued. A market order is accepted only while the
// regular session is open; a limit order is accepted in the regular session,
// or in pre-market and after-hours as well when it says ExtendedHours. Every
// order is a DAY order: it expires at the end of the last session it may
// trade in (16:00 or 20:00 New York time on its trade date).
//
// # Ex-date rule
//
// Under T+1 the exchange sets the ex-date equal to the record date when the
// record date is a trading day. In general the ex-date is the first trading
// day whose trades settle after the record date; AnnounceCorporateAction
// refuses any other ex-date. Entitlement is the trade-date position
// (WALLET plus UNSETTLED_RECEIVABLE shares) at 00:00 New York time on the
// ex-date, read from the ledger itself, which under that rule is exactly the
// settled position at the end of the record date. It is counted in the units
// of the ex-date: a split effective before the ex-date counts even when it was
// applied later (nothing trades in between), and the dividend waits until
// every such split is applied; a split effective on or after the ex-date
// never counts.
//
// # What this package must never do
//
//   - Move value outside domain SIMULATED, post a kind that is not
//     simulated-only, or post anything that does not balance per party.
//   - Execute live: no adapter in this build talks to a real broker, and
//     NewExecutionAdapter refuses provider mode live whatever the
//     environment, while fake and sandbox are refused in STAGING and PROD.
//   - Book the venue, hide a spread, charge anything the stored fee
//     disclosure does not account for, or charge anything the fee ledger
//     did not book.
//   - Let shares or cash be spent twice: every order is backed by a capital
//     reservation on the WALLET it will draw from, and the ledger refuses a
//     negative WALLET.
//   - Apply a fill, a settlement or a corporate action twice: each is an
//     econ effect under a deterministic key, and the database holds one
//     fill per order, one settlement per fill and one application per
//     account per action.
//   - Change an order or corporate-action status without a transition row,
//     edit fills or applications, record a fill its order does not end
//     FILLED with, or settle a settlement twice (the database refuses each).
//   - Let an agent apply an effect: agents propose intents only, and an
//     agent's order without delegated authority the AuthorityReader vouches
//     for is refused by the risk kernel.
//   - Create a settlement asset: the sandbox dollar is the platform's, and
//     every listing is given it.
//   - Send an order to the venue before it is recorded, or retry a
//     submission whose outcome is UNKNOWN: it is resolved by polling.
//   - Use floating point, read the wall clock, or hold mutable state at
//     package level.
//   - Let a settlement that cannot settle stop the settlement of any other,
//     or take it again before its backoff has passed.
package equities
