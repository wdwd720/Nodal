// Package prediction is the prediction ledger and its calibration (PARTS 72,
// 73). A prediction is committed before the outcome is knowable, is immutable
// once written, and is the precondition for any autonomous trade: the intent
// service, a database trigger and a foreign key all require it.
//
// What this package must never do:
//
//   - It never amends or deletes a prediction. There is no update statement
//     in this package, the predictions and prediction_outcomes tables carry
//     forbid_mutation triggers, and cp_app holds no UPDATE or DELETE grant on
//     either. A prediction that could be edited after the fact is worthless.
//   - It never scores a prediction against knowledge that was not available
//     when the prediction was made. An outcome is resolved from prices at or
//     after the horizon end, the prediction's own information set is hashed at
//     commit time, and decision_available_at <= committed_at is a CHECK.
//   - It never equates return with calibration. Realized return and
//     calibration scores are separate columns and are never combined into one
//     number (PART 73).
//   - It holds no authority: it does not sign, move money, reserve capital or
//     change a policy, and it does not import internal/{signing,wallet,admin,
//     gates,withdrawal,killswitch,capital,execution}.
package prediction
