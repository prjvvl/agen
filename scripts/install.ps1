# Installs agen + agen-host into $env:AGEN_INSTALL_DIR (default ~\.agen\bin):
#   irm https://prjvvl.github.io/agen/install.ps1 | iex       # latest release
#   $env:AGEN_VERSION = "v0.1.1"; irm .../install.ps1 | iex   # a given release
#   .\install.ps1 <archive.zip|URL> [-Sha256 HEX]              # a given archive
#   .\install.ps1 -Uninstall [-Purge]   # -Purge also deletes ~\.agen data
# Archives are checked against -Sha256, or else <archive>.sha256 next to them
# (required for URLs). Unless AGEN_NO_MODIFY_PATH=1, the install directory is
# added to your user PATH.
param([string]$Archive, [string]$Sha256, [switch]$Uninstall, [switch]$Purge)
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$repo = "https://github.com/prjvvl/agen"
$dir = if ($env:AGEN_INSTALL_DIR) { $env:AGEN_INSTALL_DIR } else { Join-Path $HOME ".agen\bin" }
$agenHome = if ($env:AGEN_HOME) { $env:AGEN_HOME } else { Join-Path $HOME ".agen" }

# Deletes AGEN_HOME only when it is clearly Agen's own directory.
function Remove-AgenHome {
  if (-not (Test-Path $agenHome)) { return }
  $full = (Resolve-Path $agenHome).Path.TrimEnd('\')
  $unsafe = @((Resolve-Path $HOME).Path.TrimEnd('\'), [IO.Path]::GetPathRoot($full).TrimEnd('\'))
  if ($unsafe -contains $full) { throw "refusing to delete $full (AGEN_HOME)" }
  $markers = "agen.db", "local.json", "nest", "nests" | Where-Object { Test-Path (Join-Path $full $_) }
  if (-not $markers -and (Get-ChildItem -Force $full)) {
    throw "refusing to delete ${full}: it does not look like an Agen home (no agen.db, local.json or nest dirs)"
  }
  Remove-Item -Recurse -Force $full
}

function Get-UserPath { @([Environment]::GetEnvironmentVariable("Path", "User") -split ';' | Where-Object { $_ }) }

if ($Uninstall) {
  $agen = Join-Path $dir "agen.exe"
  if (Test-Path $agen) { try { & $agen down 2>$null | Out-Null } catch {} }
  Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $dir "agen.exe"), (Join-Path $dir "agen-host.exe")
  if ((Test-Path $dir) -and -not (Get-ChildItem $dir)) { Remove-Item $dir }
  $userPath = Get-UserPath
  if ($userPath -contains $dir) {
    [Environment]::SetEnvironmentVariable("Path", (($userPath | Where-Object { $_ -ne $dir }) -join ';'), "User")
  }
  if ($Purge) { Remove-AgenHome }
  "agen uninstalled"; return
}
if (-not $Archive) {
  $version = $env:AGEN_VERSION
  if (-not $version) { $version = (Invoke-RestMethod "https://api.github.com/repos/prjvvl/agen/releases/latest").tag_name }
  if ($version -notmatch '^v') { throw "could not determine the latest release (got '$version')" }
  if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "no Windows arm64 build yet; installing the amd64 build, which runs under emulation" }
  $Archive = "$repo/releases/download/$version/agen_${version}_windows_amd64.zip"
}
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("agen-" + [guid]::NewGuid())
New-Item -ItemType Directory $tmp | Out-Null
try {
  if ($Archive -match '^https?://') {
    "downloading $Archive"
    $zip = Join-Path $tmp "agen.zip"
    Invoke-WebRequest -UseBasicParsing $Archive -OutFile $zip
    if (-not $Sha256) {
      $sum = Join-Path $tmp "agen.zip.sha256"
      try { Invoke-WebRequest -UseBasicParsing "$Archive.sha256" -OutFile $sum }
      catch { throw "no $Archive.sha256: pass -Sha256" }
      $Sha256 = ((Get-Content -Raw $sum) -split '\s+')[0]
    }
    $Archive = $zip
  } elseif (-not $Sha256 -and (Test-Path "$Archive.sha256")) {
    $Sha256 = ((Get-Content -Raw "$Archive.sha256") -split '\s+')[0]
  }
  if ($Sha256) {
    $got = (Get-FileHash -Algorithm SHA256 $Archive).Hash.ToLower()
    if ($got -ne $Sha256.ToLower()) { throw "checksum mismatch for ${Archive}: got $got, want $Sha256" }
  }
  Expand-Archive -Path $Archive -DestinationPath $tmp
  New-Item -ItemType Directory -Force $dir | Out-Null
  $pkg = Get-ChildItem $tmp -Directory -Filter "agen_*" | Select-Object -First 1
  Copy-Item (Join-Path $pkg.FullName "agen.exe"), (Join-Path $pkg.FullName "agen-host.exe") $dir -Force
  "installed $(& (Join-Path $dir 'agen.exe') version) to $dir"
  if (($env:PATH -split ';') -notcontains $dir) {
    if ($env:AGEN_NO_MODIFY_PATH -ne "1") {
      if ((Get-UserPath) -notcontains $dir) {
        [Environment]::SetEnvironmentVariable("Path", ((@(Get-UserPath) + $dir) -join ';'), "User")
      }
      $env:PATH = "$env:PATH;$dir"
      "added $dir to your user PATH (new terminals pick it up)"
    } else {
      "add $dir to PATH"
    }
  }
  "next: agen up"
} finally { Remove-Item -Recurse -Force $tmp }
