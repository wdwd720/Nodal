<#
.SYNOPSIS
  Verifies and completes the local half of the Nodal AWS bootstrap.

.DESCRIPTION
  Run it as many times as you like. It works out where the bootstrap has got to
  and either finishes it or names the single thing a person has to do next.

  What it will not do, because AWS does not allow it to be done safely:

  - Enrol an MFA device from the CLI. `aws iam create-virtual-mfa-device`
    returns the shared secret, and there is no way to get that onto your phone
    without it existing in a file, a variable, a terminal buffer or a shell
    history first. The console shows a QR code that goes from screen to phone
    and is never written down. So enrolment is one console instruction.
  - Read your MFA code through the AWS CLI. On Windows that prompt reads the
    console directly and hands back a corrupted value, so the code is read here
    and passed to the AWS SDK in-process instead.
  - Weaken the MFA condition. If assuming the role fails for want of MFA, this
    reports it and stops. It never edits a policy to make a failure go away.

.EXAMPLE
  .\scripts\aws\nodal-bootstrap.ps1
#>
[CmdletBinding()]
param(
    # Skip the role-assumption test, for when you only want the state report.
    [switch]$SkipRoleTest
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'NodalAws.psm1') -Force
$cfg = Get-NodalConfig

Write-Host ''
Write-Host "Nodal AWS bootstrap -- account $($cfg.AccountId), region $($cfg.Region)" -ForegroundColor White
Write-Host ''

# --- 1. the tool ------------------------------------------------------------
Write-Step 'checking the AWS CLI'
[void](Assert-AwsCli)

# --- 2. profiles, before anything that needs them ---------------------------
# The operator profile is what every check below authenticates with.
Write-Step 'writing CLI profiles'
Set-NodalProfiles

# --- 3. who are we ----------------------------------------------------------
Write-Step 'checking the operator session'
$identity = Assert-Account -ProfileName $cfg.OperatorProf
if (-not $identity) {
    Write-Action -Title 'sign in as the operator' -Detail @(
        'The operator has no live CLI session. In a browser, sign in at',
        "  https://$($cfg.AccountId).signin.aws.amazon.com/console",
        "as $($cfg.OperatorUser), then run:",
        "  aws login --profile $($cfg.OperatorProf) --region $($cfg.Region)",
        'Then run this script again.'
    )
    exit 2
}
if ($identity.Arn -ne $cfg.UserArn) {
    Write-Bad "the '$($cfg.OperatorProf)' profile is $($identity.Arn), not $($cfg.UserArn)."
    Write-Action -Title 'sign the browser in as the operator, not somebody else' -Detail @(
        '`aws login` copies whichever console session the browser already holds.',
        "Sign out, sign in as $($cfg.OperatorUser), then run:",
        "  aws login --profile $($cfg.OperatorProf) --region $($cfg.Region)"
    )
    exit 2
}
Write-Ok "operator session is $($identity.Arn)"

# --- 4. the MFA device ------------------------------------------------------
Write-Step 'looking for the operator MFA device'
$serial = Get-OperatorMfaSerial -ProfileName $cfg.OperatorProf

if (-not $serial) {
    # One instruction, not a sequence. The device name is the part that matters:
    # the operator's own permission to enrol is scoped to
    # arn:aws:iam::<account>:mfa/${aws:username}, so any other name is denied by
    # the policy rather than merely being untidy.
    Write-Action -Title 'enrol the operator MFA device' -Detail @(
        'Open this page and use "Assign MFA device":',
        "  https://console.aws.amazon.com/iam/home#/users/details/$($cfg.OperatorUser)?section=security_credentials",
        '',
        "  Device name   $($cfg.OperatorUser)      <- must be exactly this",
        '  Device type   Authenticator app',
        '',
        'Scan the QR code and enter two consecutive codes. Then sign out of the',
        'console, sign back in as the operator completing the MFA challenge, and',
        'run this script again. It will do the rest.'
    )
    exit 3
}

if ($serial -ne $cfg.MfaArn) {
    Write-Bad "the device is $serial"
    Write-Action -Title 'rename the MFA device' -Detail @(
        "The device must be named exactly '$($cfg.OperatorUser)', because the",
        "operator's own enrolment permission is scoped to $($cfg.MfaArn).",
        'Remove the device on the security credentials page and assign a new one',
        'with that exact name.'
    )
    exit 3
}
Write-Ok "MFA device is $serial"

# --- 5. and AWS agrees it is active ----------------------------------------
$devs = Invoke-AwsQuiet @('iam', 'list-mfa-devices',
    '--user-name', $cfg.OperatorUser, '--profile', $cfg.OperatorProf,
    '--region', $cfg.Region, '--output', 'json')
$enabled = $null
if ($devs.Ok) {
    $doc = ($devs.Output | Out-String | ConvertFrom-Json)
    if ($doc.MFADevices.Count -gt 0) {
        $enabled = ([datetime]$doc.MFADevices[0].EnableDate).ToUniversalTime()
        Write-Ok "mfa_active=true, enrolled $($enabled.ToString('u'))"
    }
}

# --- 6. is the session older than the device? -------------------------------
# A console session created before enrolment never passed an MFA challenge, so
# nothing it produces can carry the claim. Saying so here saves a confusing
# AccessDenied later.
if ($enabled -and (Test-LoginPredatesMfa -MfaEnabledUtc $enabled)) {
    Write-Warn 'the cached login session is older than the MFA device'
    Write-Action -Title 'sign in once more, with MFA' -Detail @(
        'Your current session was created before the device existed, so it never',
        'passed an MFA challenge. Sign out of the console, sign back in as',
        "$($cfg.OperatorUser) completing the MFA prompt, then run:",
        "  aws login --profile $($cfg.OperatorProf) --region $($cfg.Region)",
        'Then run this script again.'
    )
    exit 4
}

# --- 7. the role profile ----------------------------------------------------
# Rewritten on every run, so a profile left over from the role_arn + mfa_serial
# approach is replaced rather than merely added to.
Write-Step 'pointing the role profile at the session helper'
Set-NodalProfiles

if ($SkipRoleTest) {
    Write-Warn 'skipping the role test as asked'
    exit 0
}

# --- 8. the whole chain -----------------------------------------------------
Write-Step 'assuming the deployment role'
$arn = Enter-NodalRole
if (-not $arn) {
    Write-Bad 'the role was not assumed'
    Write-Host ''
    Write-Host '  The MFA requirement stays. It is not the thing to change.' -ForegroundColor Yellow
    Write-Host '  Re-run and check the code was current; codes expire in 30 seconds.' -ForegroundColor Yellow
    Write-Host "  If it keeps failing, run with -Verbose and check that $($cfg.MfaArn)" -ForegroundColor Yellow
    Write-Host '  is the device your authenticator actually holds.' -ForegroundColor Yellow
    exit 5
}
Assert-RoleIdentity -Arn $arn

Write-Host ''
Write-Host '  Bootstrap complete. From now on:' -ForegroundColor Green
Write-Host '    .\scripts\aws\nodal-login.ps1     once per working session' -ForegroundColor Green
Write-Host '    .\scripts\aws\nodal-tf.ps1 plan   to run Terraform as the role' -ForegroundColor Green
Write-Host ''
exit 0
