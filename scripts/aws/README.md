# scripts/aws

Three scripts, so that deploying does not mean reading commands out of a chat
window and pasting them back.

| Script | When | Human input |
|---|---|---|
| `nodal-bootstrap.ps1` | once, and again whenever something looks wrong | at most one action, named on screen |
| `nodal-login.ps1` | once per working session | one MFA code |
| `nodal-tf.ps1` | every Terraform run | none |

```powershell
.\scripts\aws\nodal-bootstrap.ps1     # verify and finish the local setup
.\scripts\aws\nodal-login.ps1         # one MFA code, good for the hour
.\scripts\aws\nodal-tf.ps1 plan       # runs as the role, no prompt
```

## What they refuse to do

**Enrol an MFA device from the CLI.** `aws iam create-virtual-mfa-device`
returns the shared secret in its response. Getting that onto a phone means it
first exists in a variable, a file, a terminal buffer or a shell history, and a
TOTP seed is a permanent credential rather than a momentary one. The console
draws a QR code that goes from screen to phone and is never written down, so
enrolment is one console instruction and nothing more.

**Weaken the MFA requirement.** If the role cannot be assumed for want of MFA,
the scripts say so and stop. None of them edits a policy to make a failure go
away. `nodal-tf.ps1` additionally refuses to run at all unless the caller is an
assumed-role session for `nodal-terraform`, so a misconfigured profile that
quietly resolves to root fails on the first line instead of provisioning
something.

**Write a credential anywhere.** No access keys exist. The role session lives in
the AWS CLI's own credential cache, which is what lets one MFA code cover an
hour of work for both a person and an agent. `nodal-login.ps1` puts the same
session into the shell's environment; `nodal-tf.ps1` reads it back out with
`aws configure export-credentials` for the lifetime of one child process and
restores whatever was there before.

## How the MFA code is handled

`Read-Host -AsSecureString`, so it is not echoed. It is then piped to the AWS
CLI's own stdin prompt rather than passed as `--token-code`, because a
command-line argument is visible to anything that can list processes. The
decrypted string is freed immediately afterwards. It reaches no file, no log and
no history.

## Why `aws login` is not enough on its own

`aws login` mints credentials that mirror a console session, and those
credentials do **not** carry `aws:MultiFactorAuthPresent`. This was established
with the IAM policy simulator rather than inferred: the same request is
`allowed` with the claim present and `implicitDeny` without it.

So the role profile sets `mfa_serial`, which makes the CLI pass `SerialNumber`
and `TokenCode` to `sts:AssumeRole` itself. That is the mechanism AWS documents
for a trust policy that tests for MFA, and it satisfies the condition rather
than avoiding it.

## Exit codes

`nodal-bootstrap.ps1` uses them, so a wrapper can tell "not finished" from
"broken":

| Code | Meaning |
|---|---|
| 0 | done |
| 2 | no usable operator session; sign in and `aws login` |
| 3 | no MFA device, or one with the wrong name |
| 4 | the login session predates the MFA device; sign in again |
| 5 | the role was not assumed |
