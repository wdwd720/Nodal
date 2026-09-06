// Package wallettest is the in-process WalletProvider/SigningProvider test
// double. It derives a deterministic ed25519 key per provider wallet id,
// really signs transactions with it, records every call, and can be told to
// fail or hang. The constructor refuses STAGING and PROD (PART 97); production
// wiring must never import this package (lintfin, depguard).
package wallettest
