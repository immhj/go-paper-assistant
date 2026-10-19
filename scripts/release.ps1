<#
.SYNOPSIS
  Build images locally, ship them to the server as a tarball, and restart the stack.
.DESCRIPTION
  The server (mainland China cloud) cannot reliably pull from Docker Hub, and this workstation
  sits on an internal network, so images travel as `docker save` archives over scp.
  Images are tagged with the git short SHA so any shipped version can be restored.
  A normal release requires the GitHub Actions CI run for this exact HEAD to be successful;
  -SkipCI is an explicit emergency-only escape hatch. After activation, a read-only HTTPS
  health check passes through Caddy over an SSH tunnel without creating product data or using
  the model. Server-side steps live in deploy/remote.sh.
.EXAMPLE
  ./scripts/release.ps1 -Server deploy@203.0.113.10 -PublicIP 203.0.113.10 -IncludeBaseImages
  ./scripts/release.ps1 -Server deploy@203.0.113.10 -PublicIP 203.0.113.10
  ./scripts/release.ps1 -Server deploy@203.0.113.10 -Rollback 9934ac3
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $Server,
    [string] $RemoteDir = '/opt/paper-assistant',
    # Server public IPv4. Required for a normal release so the post-release smoke keeps the
    # certificate Host/SNI rather than bypassing Caddy through an app port.
    [string] $PublicIP,
    # Local-only SSH tunnel port used for the Caddy HTTPS health smoke.
    [ValidateRange(1024, 65535)] [int] $SmokePort = 18443,
    # Also ship mysql/redis/caddy: needed on the first release or after bumping their versions.
    [switch] $IncludeBaseImages,
    # Skip the exact-HEAD GitHub Actions CI gate. Emergency use only; it is deliberately noisy.
    [switch] $SkipCI,
    # Switch the server to an already-shipped tag instead of building.
    [string] $Rollback,
    # Shipped tags to keep on the server for rollback.
    [int] $Keep = 3
)

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)

# Keep in sync with docker-compose.yml / docker-compose.prod.yml.
$baseImages = 'mysql:8.4.6', 'redis:7.4.1-alpine', 'caddy:2.11.7-alpine'

function Step([string] $what, [scriptblock] $block) {
    Write-Host "==> $what" -ForegroundColor Cyan
    # docker/ssh write progress to stderr. Under Windows PowerShell 5.1 with 'Stop', redirected
    # native stderr becomes a terminating error, so judge native commands by exit code only.
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { & $block } finally { $ErrorActionPreference = $previous }
    if ($LASTEXITCODE -ne 0) { throw "$what failed (exit $LASTEXITCODE)" }
}
function Remote([string] $what, [string] $arguments) {
    Step $what { ssh $Server "bash '$RemoteDir/deploy/remote.sh' $arguments" }
}
function Assert-CIForHead([string] $headSHA) {
    Step "verify GitHub Actions CI for HEAD $headSHA" {
        $raw = gh run list --workflow ci.yml --commit $headSHA --limit 100 --json databaseId,headSha,status,conclusion,createdAt
        if ($LASTEXITCODE -ne 0) { throw 'gh could not query GitHub Actions; authenticate gh and retry, or use -SkipCI deliberately.' }
        $runs = @($raw | ConvertFrom-Json)
        $run = @($runs | Where-Object { $_.headSha -eq $headSHA } | Sort-Object createdAt -Descending | Select-Object -First 1)
        if ($run.Count -ne 1) {
            throw "No CI workflow run found for current HEAD $headSHA. Push this exact commit and wait for CI, or use -SkipCI deliberately."
        }
        if ($run[0].status -ne 'completed' -or $run[0].conclusion -ne 'success') {
            throw "CI run $($run[0].databaseId) for HEAD $headSHA is status=$($run[0].status), conclusion=$($run[0].conclusion); deployment is blocked."
        }
    }
}
function Get-RemoteImageTag() {
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $value = @(ssh $Server "sed -n 's/^IMAGE_TAG=//p' '$RemoteDir/.env'") } finally { $ErrorActionPreference = $previous }
    if ($LASTEXITCODE -ne 0) { throw 'could not read the active remote image tag' }
    if ($value.Count -gt 0) { return ([string]$value[0]).Trim() }
    return ''
}
function Invoke-RemoteHealthSmoke([string] $publicIP) {
    $tunnel = $null
    try {
        $tunnel = Start-Process -FilePath ssh -ArgumentList @(
            '-o', 'BatchMode=yes', '-o', 'ExitOnForwardFailure=yes', '-N',
            '-L', "127.0.0.1:${SmokePort}:127.0.0.1:443", $Server
        ) -PassThru -NoNewWindow
        Start-Sleep -Seconds 1
        if ($tunnel.HasExited) { throw "SSH smoke tunnel exited with code $($tunnel.ExitCode)" }
        # curl runs on the Windows host, which owns the loopback-only SSH listener. --connect-to
        # changes TCP destination only; URL Host, TLS SNI and IP-certificate verification remain publicIP.
        Step 'smoke remote HTTPS health through Caddy' {
            $body = curl.exe --fail --silent --show-error --connect-to "${publicIP}:443:127.0.0.1:${SmokePort}" "https://${publicIP}/healthz"
            if ($LASTEXITCODE -ne 0) { throw 'curl could not reach the tunneled Caddy HTTPS endpoint' }
            $health = $body | ConvertFrom-Json
            if ($health.status -ne 'ok') { throw "unexpected health response: $body" }
        }
    }
    finally {
        if ($null -ne $tunnel -and -not $tunnel.HasExited) {
            Stop-Process -Id $tunnel.Id -ErrorAction Stop
            $tunnel.WaitForExit()
        }
    }
}

if ($Rollback) {
    Remote "activate $Rollback" "activate $Rollback"
    Write-Host "Rolled back to $Rollback" -ForegroundColor Green
    return
}
if ([string]::IsNullOrWhiteSpace($PublicIP)) { throw 'PublicIP is required for a normal release.' }
if (git status --porcelain) { throw 'Working tree is dirty: commit first so the image tag identifies the code.' }
$headSHA = (git rev-parse HEAD).Trim()
$tag = (git rev-parse --short HEAD).Trim()
if ($SkipCI) {
    Write-Warning "Skipping CI gate for HEAD $headSHA. Use only for an emergency release."
} else {
    Assert-CIForHead $headSHA
}

# --- Build -------------------------------------------------------------------------------
$env:IMAGE_TAG = $tag
$env:DOCKER_DEFAULT_PLATFORM = 'linux/amd64'
Step "build app + paperscraper images ($tag, linux/amd64)" { docker compose build app paperscraper }
$images = @("paper-assistant-app:$tag", "paper-assistant-paperscraper:$tag")
if ($IncludeBaseImages) {
    foreach ($image in $baseImages) { Step "pull $image" { docker pull --platform linux/amd64 $image } }
    $images += $baseImages
}

# --- Ship --------------------------------------------------------------------------------
New-Item -ItemType Directory -Force release | Out-Null
$archive = "release/paper-assistant-$tag.tar"
Step "save $($images.Count) images -> $archive" { docker save -o $archive @images }
Write-Host ('    {0:N0} MiB' -f ((Get-Item $archive).Length / 1MB))

Step 'create remote directories' { ssh $Server "mkdir -p '$RemoteDir/deploy' '$RemoteDir/release' '$RemoteDir/backups'" }
Step 'upload compose files' { scp docker-compose.yml docker-compose.prod.yml "${Server}:$RemoteDir/" }
Step 'upload deploy scripts and Caddyfile' {
    scp deploy/Caddyfile deploy/remote.sh deploy/backup.sh "${Server}:$RemoteDir/deploy/"
}
Step 'upload image archive' { scp $archive "${Server}:$RemoteDir/release/" }
Remote 'load images' "load '$RemoteDir/release/paper-assistant-$tag.tar'"

# --- Activate ----------------------------------------------------------------------------
$previousTag = Get-RemoteImageTag
try {
    Remote "activate $tag" "activate $tag"
    Invoke-RemoteHealthSmoke $PublicIP
    Remote 'prune old releases' "prune $Keep $tag"
}
catch {
    if ($previousTag) {
        Write-Warning "Deployment did not pass activation/smoke. Roll back explicitly after diagnosing the failure: ./scripts/release.ps1 -Server $Server -Rollback $previousTag"
    }
    throw
}
finally {
    if (Test-Path $archive) { Remove-Item $archive }
}
Write-Host "Released $tag to $Server" -ForegroundColor Green
