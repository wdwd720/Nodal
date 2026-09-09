<#
.SYNOPSIS
  Run Terraform as the nodal-terraform role, without a prompt and without
  copying credentials anywhere.

.DESCRIPTION
  Terraform's AWS provider would happily read the nodal-terraform profile and
  do the AssumeRole itself -- and then ask for an MFA code on stdin, which makes
  it unrunnable from any automation. This script sidesteps that: it reads the
  session the AWS CLI already cached (populated by nodal-login.ps1, one MFA code
  per hour), hands it to Terraform as ordinary environment variables for the
  duration of one child process, and lets go.

  Nothing is written to disk, no long-lived key exists, and no credential is
  ever pasted by a person.

  It refuses to run if the caller is not an assumed-role session for
  nodal-terraform -- not root, not the operator, not another project.

.PARAMETER Environment
  Which environment directory under infra/terraform/environments to act on.

.EXAMPLE
  .\scripts\aws\nodal-tf.ps1 plan
.EXAMPLE
  .\scripts\aws\nodal-tf.ps1 init -Environment prod
.EXAMPLE
  .\scripts\aws\nodal-tf.ps1 apply -TfArgs @('-auto-approve')
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)][string]$Command,
    [string]$Environment = 'prod',
    # Named TfArgs, not Args: $Args is an automatic variable in PowerShell and a
    # parameter of that name shadows it in ways that surprise the next reader.
    [string[]]$TfArgs = @()
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'NodalAws.psm1') -Force
$cfg = Get-NodalConfig

$repoRoot = Resolve-Path (Join-Path $PSScriptRoot '..\..')
$envDir = Join-Path $repoRoot "infra\terraform\environments\$Environment"
if (-not (Test-Path -LiteralPath $envDir)) {
    throw "No such environment: $envDir"
}

# Prove the identity before doing anything, and prove it by asking AWS rather
# than by trusting the profile file. A misconfigured source_profile that quietly
# resolves to root is exactly the failure this check exists for.
$arn = Get-CallerArn -ProfileName $cfg.RoleProf
if (-not $arn) {
    Write-Bad 'no usable session for the deployment role'
    Write-Host '  Run .\scripts\aws\nodal-login.ps1 first (it needs one MFA code).' -ForegroundColor Yellow
    exit 3
}
Assert-RoleIdentity -Arn $arn

# Pull the cached session out as plain values for one child process. --format
# process returns JSON rather than shell text, so nothing has to be parsed out
# of a string that might contain anything.
$exported = & aws configure export-credentials --profile $cfg.RoleProf --format process
if ($LASTEXITCODE -ne 0 -or -not $exported) {
    throw 'Could not export the cached role credentials. Run nodal-login.ps1.'
}
$c = ($exported | Out-String | ConvertFrom-Json)

$saved = @{}
foreach ($k in 'AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_SESSION_TOKEN', 'AWS_PROFILE', 'AWS_REGION', 'AWS_DEFAULT_REGION') {
    $saved[$k] = [Environment]::GetEnvironmentVariable($k)
}
try {
    $env:AWS_ACCESS_KEY_ID     = $c.AccessKeyId
    $env:AWS_SECRET_ACCESS_KEY = $c.SecretAccessKey
    $env:AWS_SESSION_TOKEN     = $c.SessionToken
    $env:AWS_REGION            = $cfg.Region
    $env:AWS_DEFAULT_REGION    = $cfg.Region
    # AWS_PROFILE is cleared on purpose. If it survived, the provider would
    # prefer the profile -- and the profile is the thing that prompts.
    Remove-Item Env:\AWS_PROFILE -ErrorAction SilentlyContinue

    Write-Ok "terraform $Command in environments/$Environment as $arn"
    Push-Location $envDir
    try {
        & terraform $Command @TfArgs
        $rc = $LASTEXITCODE
    } finally {
        Pop-Location
    }
} finally {
    foreach ($k in $saved.Keys) {
        if ($null -eq $saved[$k]) {
            Remove-Item "Env:\$k" -ErrorAction SilentlyContinue
        } else {
            Set-Item "Env:\$k" $saved[$k]
        }
    }
}

exit $rc
