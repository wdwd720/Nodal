# Shared helpers for the Nodal AWS bootstrap and login scripts.
#
# These exist to end a loop: a person reading a command out of a chat window,
# pasting it, reading the output back, and deciding what to run next. Everything
# in that loop that a machine can decide, a machine decides here. What is left
# is the part AWS requires a human for -- possessing an MFA device -- and each
# script stops at exactly one such moment, says one thing, and exits.
#
# ---------------------------------------------------------------------------
# Why the AWS CLI is not in the AssumeRole path
# ---------------------------------------------------------------------------
#
# A profile carrying `mfa_serial` makes the CLI prompt for a code, and on
# Windows that prompt is broken twice over. It reads the console directly rather
# than stdin, so a code cannot be piped in and the prompt cannot be suppressed;
# and what it reads is corrupted -- one six-digit code entered at that prompt
# arrived at STS as a nine-character value containing a letter, which cannot
# ever have been a TOTP.
#
# So the code is read here, in PowerShell, without echo, checked against
# ^[0-9]{6}$ before any network call, and handed to the AWS SDK in-process
# through Use-STSRole. It never becomes a command-line argument, where anything
# able to list processes could read it.
#
# The CLI is still used for the one thing it is good at here: `aws login` mints
# the operator's own short-lived credentials from a browser session, and
# `aws configure export-credentials` reads them back. That profile carries no
# mfa_serial, so it never prompts.
#
# Windows PowerShell 5.1 compatible: no ternary, no null-coalescing, no `&&`.

Set-StrictMode -Version Latest

$script:Nodal = @{
    AccountId    = '049286562577'
    Region       = 'us-east-2'
    OperatorUser = 'nodal-operator'
    OperatorProf = 'nodal-operator'
    RoleName     = 'nodal-terraform'
    RoleProf     = 'nodal-terraform'
    SessionName  = 'nodal-local'
}
$script:Nodal.RoleArn     = "arn:aws:iam::$($script:Nodal.AccountId):role/$($script:Nodal.RoleName)"
$script:Nodal.MfaArn      = "arn:aws:iam::$($script:Nodal.AccountId):mfa/$($script:Nodal.OperatorUser)"
$script:Nodal.UserArn     = "arn:aws:iam::$($script:Nodal.AccountId):user/$($script:Nodal.OperatorUser)"
$script:Nodal.LoginPath   = Join-Path $env:USERPROFILE '.aws\login\cache'
$script:Nodal.StateDir    = Join-Path $env:LOCALAPPDATA 'Nodal'
$script:Nodal.SessionFile = Join-Path $script:Nodal.StateDir 'aws-session.json'

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

# Invoke-AwsQuiet is for probes whose failure is an answer rather than a
# problem, so the CLI's error text does not scroll past as if something broke.
#
# The ErrorActionPreference dance is not decoration. In Windows PowerShell 5.1,
# redirecting a NATIVE command's stderr wraps each line in an ErrorRecord, and
# under `$ErrorActionPreference = 'Stop'` -- which every script here sets --
# that terminates the script even when the command exited 0. So the preference
# is lowered for the call and the error records are filtered out of the result.
function Invoke-AwsQuiet {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $raw = & aws @Arguments 2>&1
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prev
    }
    $out = @($raw | Where-Object { $_ -isnot [System.Management.Automation.ErrorRecord] })
    return [pscustomobject]@{ Ok = ($code -eq 0); Output = $out }
}

# Write-PlainFile writes text with no byte-order mark.
#
# PowerShell 5.1's `-Encoding utf8` writes a BOM, and ~/.aws/config is read by
# botocore's configparser, which does not skip one: three invisible bytes at the
# top of that file turn every AWS command in every tool on the machine into
# "Unable to parse config file". Nothing else in these scripts writes a file
# another program reads, and this is why.
function Write-PlainFile {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][string[]]$Lines)
    $enc = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($Path, (($Lines -join "`r`n") + "`r`n"), $enc)
}

function Assert-AwsCli {
    $cmd = Get-Command aws -ErrorAction SilentlyContinue
    if (-not $cmd) { throw 'The AWS CLI is not on PATH. Install AWS CLI v2.' }
    $raw = (& aws --version) | Out-String
    if ($raw -notmatch 'aws-cli/(\d+)\.(\d+)\.(\d+)') { throw "Could not read a version from: $raw" }
    $major = [int]$Matches[1]
    $minor = [int]$Matches[2]
    if ($major -lt 2) { throw "AWS CLI v2 is required; found v$major." }
    # export-credentials arrived in 2.14, and reading the operator's login
    # session out of the CLI is how the SDK gets its source credentials.
    if ($major -eq 2 -and $minor -lt 14) { throw "AWS CLI 2.14+ is required; found $major.$minor." }
    Write-Ok "AWS CLI $major.$minor.$($Matches[3])"
}

# Assert-AwsSdk makes sure the module that actually assumes the role is present,
# installing it if not. AWS.Tools.SecurityToken is the modular AWS Tools for
# PowerShell package: a few megabytes rather than the whole SDK.
function Assert-AwsSdk {
    if (Get-Module -ListAvailable -Name AWS.Tools.SecurityToken) {
        Import-Module AWS.Tools.SecurityToken -ErrorAction Stop
        return
    }
    Write-Step 'installing AWS.Tools.SecurityToken for the current user'
    # TLS 1.2 is not on by default in Windows PowerShell 5.1 and the gallery
    # requires it; without this the install fails with an unhelpful error.
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Scope CurrentUser -Force -ErrorAction Stop | Out-Null
    Install-Module -Name AWS.Tools.SecurityToken -Scope CurrentUser -Force -AllowClobber -Repository PSGallery -ErrorAction Stop
    Import-Module AWS.Tools.SecurityToken -ErrorAction Stop
    Write-Ok 'AWS Tools for PowerShell installed'
}

# ---------------------------------------------------------------------------
# Identity checks.
# ---------------------------------------------------------------------------

function Get-CallerArn {
    param([Parameter(Mandatory = $true)][string]$ProfileName)
    $cfg = Get-NodalConfig
    $r = Invoke-AwsQuiet @('sts', 'get-caller-identity', '--profile', $ProfileName,
        '--region', $cfg.Region, '--query', 'Arn', '--output', 'text')
    if (-not $r.Ok) { return $null }
    return ($r.Output | Out-String).Trim()
}

# Assert-Account refuses to touch anything if the credentials in hand belong to
# a different AWS account. Every other check assumes the account is the right
# one, so this is the check that has to come first.
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

# Test-LoginPredatesMfa: a console session created before the MFA device was
# enrolled never passed an MFA challenge. The signal is the mtime of the cached
# login session, which is a proxy rather than a promise -- so an unreadable
# cache returns $false and the live test decides instead.
function Test-LoginPredatesMfa {
    param([Parameter(Mandatory = $true)][datetime]$MfaEnabledUtc)
    $cfg = Get-NodalConfig
    if (-not (Test-Path -LiteralPath $cfg.LoginPath)) { return $false }
    $newest = Get-ChildItem -LiteralPath $cfg.LoginPath -Filter '*.json' -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
    if (-not $newest) { return $false }
    return ($newest.LastWriteTimeUtc -lt $MfaEnabledUtc)
}

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

# ---------------------------------------------------------------------------
# Profiles.
# ---------------------------------------------------------------------------

# Remove-NodalProfileKey deletes keys from a profile. Setting a key to an empty
# string leaves it present, and the CLI still reads it, so the lines have to go
# rather than be blanked.
function Remove-NodalProfileKey {
    param([Parameter(Mandatory = $true)][string]$ProfileName, [Parameter(Mandatory = $true)][string[]]$Keys)
    $path = Join-Path $env:USERPROFILE '.aws\config'
    if (-not (Test-Path -LiteralPath $path)) { return }
    $lines = @(Get-Content -LiteralPath $path)
    $out = New-Object System.Collections.Generic.List[string]
    $inTarget = $false
    foreach ($line in $lines) {
        if ($line -match '^\s*\[') {
            $inTarget = ($line -match "^\s*\[profile\s+$([regex]::Escape($ProfileName))\s*\]\s*$")
            $out.Add($line)
            continue
        }
        if ($inTarget) {
            $key = ($line -split '=', 2)[0].Trim()
            if ($Keys -contains $key) { continue }
        }
        $out.Add($line)
    }
    Write-PlainFile -Path $path -Lines $out
}

# Set-NodalProfiles is idempotent: `aws configure set` writes a key to a value
# whether or not it was already that value.
#
# The role profile is a credential_process profile, NOT a role_arn profile, and
# that is the whole difference between this working and hanging. A role_arn
# profile with mfa_serial makes the CLI prompt on the console; without one it
# fails the trust policy. credential_process asks a script instead, and the
# script answers from the session this module established -- so
# `aws --profile nodal-terraform`, Terraform and every SDK work natively, none
# of them prompts, and a cold session is a fast error rather than a hang.
function Set-NodalProfiles {
    $cfg = Get-NodalConfig

    & aws configure set region $cfg.Region --profile $cfg.OperatorProf
    & aws configure set login_session $cfg.UserArn --profile $cfg.OperatorProf

    $helper = Join-Path $PSScriptRoot 'nodal-credential-process.ps1'
    & aws configure set region $cfg.Region --profile $cfg.RoleProf
    & aws configure set credential_process "powershell -NoProfile -ExecutionPolicy Bypass -File ""$helper""" --profile $cfg.RoleProf

    # Left over from the approach that could not work. If any survived, the CLI
    # would prefer them and prompt on the console again.
    Remove-NodalProfileKey -ProfileName $cfg.RoleProf -Keys @('mfa_serial', 'role_arn', 'source_profile')

    Write-Ok 'profiles configured'
}

# ---------------------------------------------------------------------------
# The session.
#
# Role credentials are stored encrypted with DPAPI, which scopes them to this
# Windows user on this machine, and outside the repository. The expiry and the
# ARN are stored in clear, because a caller has to be able to ask "is this still
# good?" without decrypting anything.
# ---------------------------------------------------------------------------

function Save-NodalSession {
    param([Parameter(Mandatory = $true)]$Credentials, [Parameter(Mandatory = $true)][string]$Arn)
    $cfg = Get-NodalConfig
    if (-not (Test-Path -LiteralPath $cfg.StateDir)) {
        New-Item -ItemType Directory -Path $cfg.StateDir -Force | Out-Null
    }
    $protect = {
        param($v)
        ConvertFrom-SecureString -SecureString (ConvertTo-SecureString -String $v -AsPlainText -Force)
    }
    $doc = [pscustomobject]@{
        arn          = $Arn
        expires      = $Credentials.Expiration.ToUniversalTime().ToString('o')
        accessKeyId  = (& $protect $Credentials.AccessKeyId)
        secretKey    = (& $protect $Credentials.SecretAccessKey)
        sessionToken = (& $protect $Credentials.SessionToken)
    }
    Write-PlainFile -Path $cfg.SessionFile -Lines @($doc | ConvertTo-Json)
}

# Get-NodalSession returns the live session, or $null. It is deliberately
# pessimistic about the last two minutes of a session's life: credentials that
# expire mid-apply are worse than being asked for a code.
function Get-NodalSession {
    $cfg = Get-NodalConfig
    if (-not (Test-Path -LiteralPath $cfg.SessionFile)) { return $null }
    try {
        $doc = Get-Content -LiteralPath $cfg.SessionFile -Raw | ConvertFrom-Json
        $expires = ([datetime]$doc.expires).ToUniversalTime()
        if ($expires -le (Get-Date).ToUniversalTime().AddMinutes(2)) { return $null }
        $reveal = {
            param($v)
            $sec = ConvertTo-SecureString -String $v
            $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec)
            try { return [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr) }
            finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
        }
        return [pscustomobject]@{
            Arn             = $doc.arn
            Expiration      = $expires
            AccessKeyId     = (& $reveal $doc.accessKeyId)
            SecretAccessKey = (& $reveal $doc.secretKey)
            SessionToken    = (& $reveal $doc.sessionToken)
        }
    } catch {
        # Written by another user, or truncated. Cold rather than broken: the
        # caller signs in again.
        return $null
    }
}

function Test-NodalSession { return ($null -ne (Get-NodalSession)) }

function Clear-NodalSession {
    $cfg = Get-NodalConfig
    if (Test-Path -LiteralPath $cfg.SessionFile) { Remove-Item -LiteralPath $cfg.SessionFile -Force }
}

# ---------------------------------------------------------------------------
# The MFA code.
# ---------------------------------------------------------------------------

# Read-MfaCode reads one code without echo and checks it locally before any
# network call. Local validation is not politeness: a corrupted read otherwise
# costs a round trip and, worse, looks like a rejected code rather than a broken
# one -- which is exactly how the CLI's own prompt hid its bug.
#
# The plaintext exists only inside this function and the caller's try block, and
# both clear it.
function Read-MfaCode {
    param([int]$Attempts = 2)
    for ($i = 1; $i -le $Attempts; $i++) {
        $secure = Read-Host -Prompt '  MFA code (hidden)' -AsSecureString
        $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
        try {
            $code = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)
        } finally {
            [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
        }
        if ($code -match '^[0-9]{6}$') { return $code }
        # The length is reported; the value never is.
        Write-Warn "that was $($code.Length) character(s); a TOTP code is exactly six digits"
        $code = $null
    }
    return $null
}

# ---------------------------------------------------------------------------
# Assuming the role.
# ---------------------------------------------------------------------------

# Enter-NodalRole assumes nodal-terraform with one MFA code and caches the
# result. The AssumeRole call happens in this process through the AWS SDK, so
# the code never reaches a command line and no external process prompts.
function Enter-NodalRole {
    param([switch]$Force)
    $cfg = Get-NodalConfig

    if (-not $Force) {
        $existing = Get-NodalSession
        if ($existing) {
            Write-Ok "role session already valid until $($existing.Expiration.ToString('u'))"
            return $existing.Arn
        }
    }

    Assert-AwsSdk

    # Source credentials: the operator's own `aws login` session. That profile
    # carries no mfa_serial, so reading it never prompts.
    $exported = & aws configure export-credentials --profile $cfg.OperatorProf --format process
    if ($LASTEXITCODE -ne 0 -or -not $exported) {
        Write-Bad "could not read the '$($cfg.OperatorProf)' session"
        return $null
    }
    $src = ($exported | Out-String | ConvertFrom-Json)

    Write-Action -Title 'enter your MFA code' -Detail @(
        "Read the current code for $($cfg.OperatorUser) from your authenticator.",
        'It is hidden as you type, checked here before it is sent, and never',
        'written to a file, a log, your shell history or a command line.'
    )
    $code = Read-MfaCode
    if (-not $code) {
        Write-Bad 'no valid code was entered'
        return $null
    }

    $r = $null
    try {
        $r = Use-STSRole -RoleArn $cfg.RoleArn -RoleSessionName $cfg.SessionName `
            -SerialNumber $cfg.MfaArn -TokenCode $code -DurationInSeconds 3600 `
            -AccessKey $src.AccessKeyId -SecretKey $src.SecretAccessKey -SessionToken $src.SessionToken `
            -Region $cfg.Region -ErrorAction Stop
    } catch {
        Write-Bad "AssumeRole refused: $($_.Exception.Message)"
        Write-Host '  The MFA requirement is not the thing to change.' -ForegroundColor Yellow
        return $null
    } finally {
        $code = $null
        Remove-Variable -Name code -ErrorAction SilentlyContinue
    }

    $arn = $r.AssumedRoleUser.Arn
    if ($arn -notlike "*assumed-role/$($cfg.RoleName)/*") {
        throw "Assumed something, and it was not the deployment role: $arn"
    }
    Save-NodalSession -Credentials $r.Credentials -Arn $arn
    return $arn
}

Export-ModuleMember -Function Get-NodalConfig, Write-Step, Write-Ok, Write-Warn, Write-Bad, Write-PlainFile,
    Write-Action, Invoke-AwsQuiet, Assert-AwsCli, Assert-AwsSdk, Get-CallerArn, Assert-Account,
    Get-OperatorMfaSerial, Test-LoginPredatesMfa, Set-NodalProfiles, Remove-NodalProfileKey,
    Save-NodalSession, Get-NodalSession, Test-NodalSession, Clear-NodalSession, Read-MfaCode,
    Enter-NodalRole, Assert-RoleIdentity
