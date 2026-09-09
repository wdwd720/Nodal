<#
.SYNOPSIS
  credential_process helper for the nodal-terraform profile.

.DESCRIPTION
  The AWS CLI, Terraform's AWS provider and every AWS SDK support a profile
  whose credentials come from running a command. This is that command: it prints
  the role session that nodal-login.ps1 established, in the documented JSON
  shape, and exits.

  It exists so the role profile does NOT need role_arn + mfa_serial. That
  arrangement makes the CLI prompt for an MFA code on the Windows console, which
  cannot be answered by anything without a console and, on this platform, reads
  a corrupted value even when a person is there to type it.

  It never prompts and never assumes a role. Cold session means exit 1 with a
  message naming the script to run, which callers surface as an error instead of
  hanging forever.

  Nothing is printed but the credentials themselves: no diagnostics, no banner.
  The caller parses stdout as JSON.
#>
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'NodalAws.psm1') -Force -DisableNameChecking

$session = Get-NodalSession
if (-not $session) {
    # stderr, so it cannot be mistaken for the JSON document.
    [Console]::Error.WriteLine('nodal: no live nodal-terraform session. Run scripts\aws\nodal-login.ps1 (one MFA code).')
    exit 1
}

[pscustomobject]@{
    Version         = 1
    AccessKeyId     = $session.AccessKeyId
    SecretAccessKey = $session.SecretAccessKey
    SessionToken    = $session.SessionToken
    Expiration      = $session.Expiration.ToString('yyyy-MM-ddTHH:mm:ssZ')
} | ConvertTo-Json -Compress
