# Nodal Terms of Service

**Status: draft, pending review by qualified counsel. This document has not been
reviewed by a lawyer and is not a final agreement.**

This draft states, in plain words, what the software actually does today. It is
written so that a reviewer can check it against the system rather than against an
intention, and so that a person reading it is not told something the code does
not do.

## 1. What Nodal is

Nodal is a control plane for an internal economy. It records balances in an
append-only ledger, prices and executes internal markets, and exposes those
records through an API and a web application.

## 2. What Nodal is not

Nodal is not a bank, a broker-dealer, a money transmitter, an exchange, or an
investment adviser, and it does not hold or transfer money on your behalf. Where
a regulated activity is required to move value, Nodal instructs a licensed
provider and records what that provider did; it does not perform the activity
itself.

## 3. Credits

Credits are an internal unit of account. They are not money, not a deposit, not
a security, not a stablecoin, and not redeemable on demand. Whether Credits may
ever be converted to money is governed by the Credits Terms and by capability
controls described there; the answer today may be "no", and no part of this
document promises otherwise.

## 4. Your account

You authenticate through Nodal's identity provider. Nodal keeps a user record,
a profile you control, and a financial account that references them. Having an
account grants no financial privilege of any kind: every capability is separately
controlled and may be unavailable to you, to your jurisdiction, or to everyone.

## 5. Closing your account

You may ask for your account to be closed at any time. The request waits out a
cooling-off period, during which you can cancel it, and is then effected by a
person. Closure ends your access; it does not delete the financial and audit
records the system is required to keep, and nothing in these terms promises
deletion of them.

## 6. Suspension

Nodal may restrict, freeze or suspend an account where a control requires it —
for example a risk limit, a compliance obligation, or a security event. Where a
restriction applies to you, the product tells you that it applies and, where it
is lawful to do so, why.

## 7. Changes

A change to this document is a new version. You will be asked to accept the new
version, and the version and the exact text you accepted are recorded.

## 8. What this draft does not yet say

Governing law, dispute resolution, limitation of liability, indemnity, and the
consumer-protection language required in each jurisdiction Nodal operates in.
Those are the parts that need counsel, and inventing them here would be worse
than their absence.
