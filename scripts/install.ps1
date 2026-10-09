# Installs agen + agen-host into $env:AGEN_INSTALL_DIR (default ~\.agen\bin)
# from a release zip (path or URL), or removes them:
#   .\install.ps1 <archive.zip|URL> [-Sha256 HEX]
#   .\install.ps1 -Uninstall [-Purge]
# The archive is checked against -Sha256, or else <archive>.sha256 next to it
# (required for URLs).
param([string]$Archive, [string]$Sha256, [switch]$Uninstall, [switch]$Purge)
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
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

if ($Uninstall) {
  $agen = Join-Path $dir "agen.exe"
  if (Test-Path $agen) { try { & $agen down | Out-Null } catch {} }
  Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $dir "agen.exe"), (Join-Path $dir "agen-host.exe")
  if ((Test-Path $dir) -and -not (Get-ChildItem $dir)) { Remove-Item $dir }
  if ($Purge) { Remove-AgenHome }
  "agen uninstalled"; exit 0
}
if (-not $Archive) { throw "usage: install.ps1 <archive.zip|URL> [-Sha256 HEX] | -Uninstall [-Purge]" }
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("agen-" + [guid]::NewGuid())
New-Item -ItemType Directory $tmp | Out-Null
try {
  if ($Archive -match '^https?://') {
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
  if (($env:PATH -split ';') -notcontains $dir) { "add $dir to PATH, then run: agen up" }
} finally { Remove-Item -Recurse -Force $tmp }
