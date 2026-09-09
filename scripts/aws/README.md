# scripts/aws

Five files, so that deploying does not mean reading commands out of a chat
window and pasting them back.

| Script | When | Human input |
|---|---|---|
| `nodal-bootstrap.ps1` | once, and again whenever something looks wrong | at most one action, named on screen |
| `nodal-login.ps1` | once per working session | one MFA code |
| `nodal-deploy.ps1` | to deploy | none |
| `nodal-tf.ps1` | any single Terraform command | none |
| `nodal-credential-process.ps1` | never run by hand | none |

```powershell
.\scripts\aws\nodal-bootstrap.ps1     # verify and finish the local setup
.\scripts\aws\nodal-login.ps1         # one MFA code, good for the hour
.\scripts\aws\nodal-deploy.ps1        # state bucket, backend, init, plan
.\scripts\aws\nodal-deploy.ps1 -Apply # and then apply
```

`nodal-deploy.ps1` is idempotent at every step, so a run after a failure resumes
rather than restarts. It creates the state bucket -- the one piece of
infrastructure Terraform cannot create, because it is where Terraform's own
state lives -- then reads the bucket's settings back rather than trusting the
writes, since a bucket that silently lost versioning looks exactly like one that
has it. It generates `backend.hcl` from the account and region it already knows,
and it refuses to invent `terraform.tfvars`, which carries the hostname, the
certificate and the identity provider.

## The AWS CLI is not in the AssumeRole path, and this is why

A profile with `role_arn` + `mfa_serial` makes the CLI ask for a code itself.
On Windows that prompt is broken in two ways.

It reads the console directly rather than stdin, so a code cannot be piped in
and the prompt cannot be suppressed. Any command against such a profile
therefore **hangs** once the cached session expires, which is fatal in CI or an
agent, and a hang rather than an error.

Worse, what it reads is corrupted. One six-digit code typed at that prompt
reached STS as a nine-character value containing a letter. A TOTP code cannot
be either of those things, so the code never arrived intact.

So the role is assumed **in-process through the AWS SDK**:

```
aws login (browser, no MFA prompt)        -> operator credentials
Read-Host -AsSecureString + ^[0-9]{6}$    -> one code, validated locally
Use-STSRole (AWS.Tools.SecurityToken)     -> role credentials
DPAPI-encrypted session file              -> reused for the hour
```

The code is hidden as typed, checked before any network call, never passed as a
command-line argument where anything listing processes could read it, and never
written to a file, a log or shell history. `AWS.Tools.SecurityToken` is
installed for the current user on first run if it is absent.

## How Terraform gets the credentials

The `nodal-terraform` profile is a **`credential_process`** profile. It carries
no `role_arn` and no `mfa_serial`, so nothing can make it prompt. It runs
`nodal-credential-process.ps1`, which prints the session that
`nodal-login.ps1` established and exits. A cold session is exit 1 with a message
naming the script to run.

That means `aws --profile nodal-terraform`, Terraform's AWS provider and every
AWS SDK all work natively, none of them prompts, and none of them hangs.

`nodal-tf.ps1` additionally refuses to run unless the caller is an assumed-role
session for `nodal-terraform`, so a profile that quietly resolves to root fails
on the first line rather than provisioning something.

## Where the session lives

`%LOCALAPPDATA%\Nodal\aws-session.json`, outside the repository, with the three
credential fields encrypted by DPAPI — scoped to this Windows user on this
machine. The expiry and the role ARN are in clear so a caller can ask "is this
still good?" without decrypting anything. Sessions last one hour, and the last
two minutes are treated as already gone: credentials that expire mid-apply are
worse than being asked for a code.

No access key exists anywhere in this design.

## What these scripts will not do

**Enrol an MFA device.** `aws iam create-virtual-mfa-device` returns the shared
secret, so getting it onto a phone means it first exists in a variable, a file
or a shell history. A TOTP seed is a permanent credential, unlike the six digits
it generates. The console's QR code goes from screen to phone and is never
written down, so enrolment is one console instruction.

**Weaken the MFA requirement.** If the role cannot be assumed, the scripts say
so and stop. None of them edits a policy to make a failure go away.

## Two Windows PowerShell traps these scripts had to survive

Both were hit for real, and both are in the code with the reason attached.

`-Encoding utf8` writes a **byte-order mark** in PowerShell 5.1. Three invisible
bytes at the top of `~/.aws/config` make every AWS tool on the machine report
"Unable to parse config file", because botocore's parser does not skip one. The
config writer uses an explicit BOM-less encoder.

Redirecting a **native** command's stderr wraps each line in an `ErrorRecord`,
and under `$ErrorActionPreference = 'Stop'` that aborts the script even when the
command exited 0. The CLI wrapper lowers the preference for the call and filters
the records out.

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
