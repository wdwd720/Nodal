<#
.SYNOPSIS
  Start a working session as the Nodal deployment role.

.DESCRIPTION
  Run this once when you sit down. It checks the operator session, assumes
  nodal-terraform with one MFA code, proves the caller is the role and nothing
  else, and leaves the shell ready for Terraform.

  Nothing is written to disk by this script. The role session lives in the AWS
  CLI's own credential cache, which is what lets every later command -- yours or
  an agent's -- reuse the one MFA challenge until it expires.

.EXAMPLE
  .\scripts\aws\nodal-login.ps1

.EXAMPLE
  .\scripts\aws\nodal-login.ps1 -Force
  Assume the role again even if the cached session is still good.
#>
[CmdletBinding()]
param(
    [switch]$Force,
    # Skip exporting AWS_* into this shell. The profile still works; only the
    # environment variables are left unset.
    [switch]$NoExport
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'NodalAws.psm1') -Force
$cfg = Get-NodalConfig

Write-Host ''
Write-Host "Nodal session -- $($cfg.RoleName) in $($cfg.AccountId)/$($cfg.Region)" -ForegroundColor White
Write-Host ''

[void](Assert-AwsCli)

# The operator's own session first. If it has lapsed there is nothing to assume
# the role from, and `aws login` is the one command that needs a browser.
$operator = Get-CallerArn -ProfileName $cfg.OperatorProf
if (-not $operator) {
    Write-Action -Title 'refresh the operator session' -Detail @(
        'The login session has expired. With the browser signed in as',
        "$($cfg.OperatorUser) (completing MFA), run:",
        "  aws login --profile $($cfg.OperatorProf) --region $($cfg.Region)",
        'Then run this script again.'
    )
    exit 2
}
if ($operator -ne $cfg.UserArn) {
    Write-Bad "the operator profile resolves to $operator"
    Write-Action -Title 'sign the browser in as the operator' -Detail @(
        '`aws login` copies whichever console session the browser holds, so this',
        'profile is currently bound to the wrong identity. Sign out, sign in as',
        "$($cfg.OperatorUser), and run `aws login --profile $($cfg.OperatorProf)` again."
    )
    exit 2
}
Write-Ok "operator is $operator"

# A missing mfa_serial means the bootstrap has not finished. Say which script to
# run rather than failing with an AccessDenied that explains nothing.
$serial = (& aws configure get mfa_serial --profile $cfg.RoleProf) 2>$null
if ($LASTEXITCODE -ne 0 -or -not $serial) {
    Write-Bad "the '$($cfg.RoleProf)' profile has no mfa_serial"
    Write-Host '  Run .\scripts\aws\nodal-bootstrap.ps1 first.' -ForegroundColor Yellow
    exit 3
}

$arn = Enter-NodalRole -Force:$Force
if (-not $arn) {
    Write-Bad 'the role was not assumed; the MFA requirement is unchanged and stays that way'
    exit 5
}
Assert-RoleIdentity -Arn $arn

if (-not $NoExport) {
    # Read the cached session out of the CLI rather than assuming the role a
    # second time, so one MFA code covers the whole hour. `$env:` writes to the
    # process environment, which persists for this PowerShell session.
    $exported = & aws configure export-credentials --profile $cfg.RoleProf --format process
    if ($LASTEXITCODE -eq 0 -and $exported) {
        $c = ($exported | Out-String | ConvertFrom-Json)
        $env:AWS_ACCESS_KEY_ID     = $c.AccessKeyId
        $env:AWS_SECRET_ACCESS_KEY = $c.SecretAccessKey
        $env:AWS_SESSION_TOKEN     = $c.SessionToken
        $env:AWS_REGION            = $cfg.Region
        $env:AWS_DEFAULT_REGION    = $cfg.Region
        Write-Ok "credentials exported to this shell, expiring $($c.Expiration)"
    } else {
        Write-Warn 'could not export credentials; use -Profile nodal-terraform instead'
    }
}

Write-Host ''
Write-Host '  Ready. Terraform can now run as the role:' -ForegroundColor Green
Write-Host '    .\scripts\aws\nodal-tf.ps1 plan' -ForegroundColor Green
Write-Host ''
exit 0
