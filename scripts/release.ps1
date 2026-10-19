<#
.SYNOPSIS
  Build images locally, ship them to the server as a tarball, and restart the stack.

.DESCRIPTION
  The server (mainland China cloud) cannot reliably pull from Docker Hub, and this workstation
  sits on an internal network, so images travel as `docker save` archives over scp.
  Images are tagged with the git short SHA so any shipped version can be restored.
  Server-side steps live in deploy/remote.sh so no shell script is quoted through ssh.

  Prerequisites on the server (once): Docker Engine with the compose plugin, SSH key login,
  and $RemoteDir/.env filled from deploy/.env.production.example (chmod 600).
  See docs/deployment.md.

.EXAMPLE
  ./scripts/release.ps1 -Server deploy@203.0.113.10 -IncludeBaseImages   # first release
  ./scripts/release.ps1 -Server deploy@203.0.113.10                      # later releases
  ./scripts/release.ps1 -Server deploy@203.0.113.10 -Rollback 9934ac3     # restore a version
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $Server,
    [string] $RemoteDir = '/opt/paper-assistant',
    # Also ship mysql/redis/caddy: needed on the first release or after bumping their versions.
    [switch] $IncludeBaseImages,
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
    & $block
    if ($LASTEXITCODE -ne 0) { throw "$what failed (exit $LASTEXITCODE)" }
}
function Remote([string] $what, [string] $arguments) {
    Step $what { ssh $Server "bash '$RemoteDir/deploy/remote.sh' $arguments" }
}

if ($Rollback) {
    Remote "activate $Rollback" "activate $Rollback"
    Write-Host "Rolled back to $Rollback" -ForegroundColor Green
    return
}

if (git status --porcelain) { throw 'Working tree is dirty: commit first so the image tag identifies the code.' }
$tag = (git rev-parse --short HEAD).Trim()

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
Remote "activate $tag" "activate $tag"
Remote 'prune old releases' "prune $Keep $tag"
Remove-Item $archive
Write-Host "Released $tag to $Server" -ForegroundColor Green
