# Shared helpers for the Nodal AWS bootstrap and login scripts.
#
# These exist to end a loop: a person reading a command out of a chat window,
# pasting it, reading the output back, and deciding what to run next. Everything
# in that loop that a machine can decide, a machine decides here. What is left
# is the part AWS requires a human for -- possessing an MFA device -- and each
# script stops at exactly one such moment, says one thing, and exits.
#
# Windows PowerShell 5.1 compatible: no ternary, no null-coalescing, no `&&`.

Set-StrictMode -Version Latest

# The account, region and names every script agrees on. They are here rather
# than in each script because a bootstrap that disagrees with a login about
# which role to assume is a failure nobody would think to look for.
$script:Nodal = @{
    AccountId     = '049286562577'
    Region        = 'us-east-2'
    OperatorUser  = 'nodal-operator'
    OperatorProf  = 'nodal-operator'
    RoleName      = 'nodal-terraform'
    RoleProf      = 'nodal-terraform'
}
$script:Nodal.RoleArn   = "arn:aws:iam::$($script:Nodal.AccountId):role/$($script:Nodal.RoleName)"
$script:Nodal.MfaArn    = "arn:aws:iam::$($script:Nodal.AccountId):mfa/$($script:Nodal.OperatorUser)"
$script:Nodal.UserArn   = "arn:aws:iam::$($script:Nodal.AccountId):user/$($script:Nodal.OperatorUser)"
$script:Nodal.LoginPath = Join-Path $env:USERPROFILE '.aws\login\cache'

function Get-NodalConfig { return $script:Nodal }

# ---------------------------------------------------------------------------
# Output. Four shapes, so a reader can tell a fact from a question at a glance.
# ---------------------------------------------------------------------------

function Write-Step { param([string]$Message) Write-Host "  $Message" -ForegroundColor DarkGray }
function Write-Ok   { param([string]$Message) Write-Host "  OK    $Message" -ForegroundColor Green }
function Write-Warn { param([string]$Message) Write-Host "  NOTE  $Message" -ForegroundColor Yellow }
function Write-Bad  { param([string]$Message) Write-Host "  FAIL  $Message" -ForegroundColor Red }

# Write-Action is the only thing in these scripts that asks for a person. It is
# deliberately loud and deliberately singular: one action, never a list.
function Write-Action {
    param([string]$Title, [string[]]$Detail)
    Write-Host ''
    Write-Host '  ------------------------------------------------------------' -ForegroundColor Cyan
    Write-Host "  ONE ACTION NEEDED: $Title" -ForegroundColor Cyan
    Write-Host '  ------------------------------------------------------------' -ForegroundColor Cyan
    foreach ($line in $Detail) { Write-Host "  $line" }
    Write-Host ''
}

# ---------------------------------------------------------------------------
# Running the CLI.
# ---------------------------------------------------------------------------

# Invoke-AwsJson runs the CLI and parses JSON. Native stderr is left alone
# rather than merged: in Windows PowerShell, redirecting a native command's
# stderr wraps each line in an ErrorRecord and reports failure even on exit 0.
function Invoke-AwsJson {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [switch]$AllowFailure
    )
    $out = & aws @Arguments
    $code = $LASTEXITCODE
    if ($code -ne 0) {
        if ($AllowFailure) { return $null }
        throw "aws $($Arguments -join ' ') exited $code"
    }
    if (-not $out) { return $null }
    return ($out | Out-String | ConvertFrom-Json)
}

# Invoke-AwsQuiet is for probes whose failure is an answer rather than a
# problem, so the CLI's error text does not scroll past as if something broke.
function Invoke-AwsQuiet {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    $out = & aws @Arguments 2>$null
    return [pscustomobject]@{ Ok = ($LASTEXITCODE -eq 0); Output = $out }
}

# ---------------------------------------------------------------------------
# Checks.
# ---------------------------------------------------------------------------

function Assert-AwsCli {
    $cmd = Get-Command aws -ErrorAction SilentlyContinue
    if (-not $cmd) { throw 'The AWS CLI is not on PATH. Install AWS CLI v2.' }
    $raw = (& aws --version) | Out-String
    if ($raw -notmatch 'aws-cli/(\d+)\.(\d+)\.(\d+)') { throw "Could not read a version from: $raw" }
    $major = [int]$Matches[1]
    $minor = [int]$Matches[2]
    if ($major -lt 2) { throw "AWS CLI v2 is required; found v$major. `aws login` and `configure export-credentials` do not exist in v1." }
    # export-credentials arrived in 2.14. The login/Terraform handoff depends on
    # it, so an older v2 fails here rather than three steps later.
    if ($major -eq 2 -and $minor -lt 14) { throw "AWS CLI 2.14+ is required for `aws configure export-credentials`; found $major.$minor." }
    Write-Ok "AWS CLI $major.$minor.$($Matches[3])"
    return $raw.Trim()
}

# Get-CallerArn returns the ARN a profile currently resolves to, or $null. It
# never throws: "not signed in" is a state these scripts handle, not an error.
function Get-CallerArn {
    param([Parameter(Mandatory = $true)][string]$ProfileName)
    $cfg = Get-NodalConfig
    $r = Invoke-AwsQuiet @('sts', 'get-caller-identity', '--profile', $ProfileName,
        '--region', $cfg.Region, '--query', 'Arn', '--output', 'text')
    if (-not $r.Ok) { return $null }
    return ($r.Output | Out-String).Trim()
}

# Assert-Account refuses to touch anything if the credentials in hand belong to
# a different AWS account. Every other check in these scripts assumes the
# account is the right one, so this is the check that has to come first.
function Assert-Account {
    param([Parameter(Mandatory = $true)][string]$ProfileName)
    $cfg = Get-NodalConfig
    $r = Invoke-AwsQuiet @('sts', 'get-caller-identity', '--profile', $ProfileName,
        '--region', $cfg.Region, '--output', 'json')
    if (-not $r.Ok) { return $null }
    $id = ($r.Output | Out-String | ConvertFrom-Json)
    if ($id.Account -ne $cfg.AccountId) {
        throw "Profile '$ProfileName' is in account $($id.Account); this repository provisions only $($cfg.AccountId). Refusing to continue."
    }
    return $id
}

# Get-OperatorMfaSerial asks IAM what MFA device the operator has. The operator
# can answer this about itself: its inline policy grants iam:ListMFADevices on
# its own user, which is why the bootstrap does not need root to get here.
function Get-OperatorMfaSerial {
    param([string]$ProfileName)
    $cfg = Get-NodalConfig
    if (-not $ProfileName) { $ProfileName = $cfg.OperatorProf }
    $r = Invoke-AwsQuiet @('iam', 'list-mfa-devices', '--user-name', $cfg.OperatorUser,
        '--profile', $ProfileName, '--region', $cfg.Region,
        '--query', 'MFADevices[0].SerialNumber', '--output', 'text')
    if (-not $r.Ok) { return $null }
    $serial = ($r.Output | Out-String).Trim()
    if (-not $serial -or $serial -eq 'None') { return $null }
    return $serial
}

# Test-LoginPredatesMfa answers requirement 6: a console session that was
# created before the MFA device was enrolled did not pass an MFA challenge, and
# no amount of retrying will make it carry one. The signal is the mtime of the
# cached login session, which is a proxy rather than a promise -- so an
# unreadable cache returns $false and the script relies on the live test
# instead of guessing.
function Test-LoginPredatesMfa {
    param([Parameter(Mandatory = $true)][datetime]$MfaEnabledUtc)
    $cfg = Get-NodalConfig
    if (-not (Test-Path -LiteralPath $cfg.LoginPath)) { return $false }
    $newest = Get-ChildItem -LiteralPath $cfg.LoginPath -Filter '*.json' -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
    if (-not $newest) { return $false }
    return ($newest.LastWriteTimeUtc -lt $MfaEnabledUtc)
}

# ---------------------------------------------------------------------------
# Profiles.
# ---------------------------------------------------------------------------

# Set-NodalProfiles is idempotent by construction: `aws configure set` writes a
# key to a value whether or not it was already that value, so running the
# bootstrap twice changes nothing the second time.
function Set-NodalProfiles {
    param([string]$MfaSerial)
    $cfg = Get-NodalConfig

    & aws configure set region $cfg.Region --profile $cfg.OperatorProf
    & aws configure set login_session $cfg.UserArn --profile $cfg.OperatorProf

    & aws configure set region $cfg.Region --profile $cfg.RoleProf
    & aws configure set role_arn $cfg.RoleArn --profile $cfg.RoleProf
    & aws configure set source_profile $cfg.OperatorProf --profile $cfg.RoleProf

    if ($MfaSerial) {
        & aws configure set mfa_serial $MfaSerial --profile $cfg.RoleProf
        Write-Ok "profiles configured, role profile gated on $MfaSerial"
    } else {
        # Deliberately not written as an empty string: a profile carrying
        # `mfa_serial =` with no value is worse than one without the key, because
        # the CLI treats it as a device named nothing.
        Write-Ok 'profiles configured (mfa_serial pending, no device enrolled yet)'
    }
}

# ---------------------------------------------------------------------------
# Assuming the role.
# ---------------------------------------------------------------------------

# Enter-NodalRole populates the CLI's own credential cache for the role
# profile, so that every later `aws --profile nodal-terraform` and every
# Terraform run reuses one MFA challenge instead of asking again.
#
# The token code is read without echo and piped to the CLI's own prompt. Piping
# rather than passing --token-code is the point: a command-line argument is
# visible to anything that can list processes, and a TOTP code is a credential
# for the thirty seconds it lives.
function Enter-NodalRole {
    param([switch]$Force)
    $cfg = Get-NodalConfig

    if (-not $Force) {
        $arn = Get-CallerArn -ProfileName $cfg.RoleProf
        if ($arn -and $arn -like "*assumed-role/$($cfg.RoleName)/*") {
            Write-Ok "role session already valid: $arn"
            return $arn
        }
    }

    Write-Action -Title 'enter your MFA code' -Detail @(
        "Open your authenticator and read the current code for $($cfg.OperatorUser).",
        'It is not echoed, not logged, and not written anywhere.'
    )
    $secure = Read-Host -Prompt '  MFA code' -AsSecureString
    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
    try {
        $code = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)
        if ($code -notmatch '^\d{6}$') { throw 'An MFA code is six digits.' }
        # The CLI prompts on stdin for the mfa_serial code; feeding it there
        # keeps the code out of argv and out of PowerShell history.
        $out = $code | & aws sts get-caller-identity --profile $cfg.RoleProf --region $cfg.Region --query Arn --output text
        $rc = $LASTEXITCODE
    } finally {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
        Remove-Variable -Name code -ErrorAction SilentlyContinue
    }

    if ($rc -ne 0) { return $null }
    $arn = ($out | Out-String).Trim()
    if ($arn -notlike "*assumed-role/$($cfg.RoleName)/*") {
        throw "Assumed something, and it was not the deployment role: $arn"
    }
    return $arn
}

# Assert-RoleIdentity is requirement 11 as a function. Terraform must run as the
# role and as nothing else -- not root, not the operator, not the other
# project's identity -- and the check is by ARN shape rather than by trusting
# that the profile was configured correctly.
function Assert-RoleIdentity {
    param([Parameter(Mandatory = $true)][string]$Arn)
    $cfg = Get-NodalConfig
    if ($Arn -like '*:root') { throw "Refusing to proceed: the caller is root ($Arn)." }
    if ($Arn -like '*bdg-deployer*') { throw "Refusing to proceed: the caller belongs to another project ($Arn)." }
    if ($Arn -notlike "*assumed-role/$($cfg.RoleName)/*") {
        throw "Refusing to proceed: expected an assumed-role session for $($cfg.RoleName), got $Arn."
    }
    Write-Ok "caller is $Arn"
}

Export-ModuleMember -Function Get-NodalConfig, Write-Step, Write-Ok, Write-Warn, Write-Bad,
    Write-Action, Invoke-AwsJson, Invoke-AwsQuiet, Assert-AwsCli, Get-CallerArn, Assert-Account,
    Get-OperatorMfaSerial, Test-LoginPredatesMfa, Set-NodalProfiles, Enter-NodalRole,
    Assert-RoleIdentity
