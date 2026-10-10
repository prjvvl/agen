# Install check on Windows: install a release zip into a temp dir, agen up, run
# a task, agen down, uninstall --purge; fails if anything is left running,
# registered or on disk. Also checks the installer's safety rails (checksum,
# purge refusal). Usage: .\scripts\test-install.ps1 dist\agen_<v>_windows_amd64.zip
param([Parameter(Mandatory)][string]$Archive)
$ErrorActionPreference = "Stop"
$base = Join-Path $env:TEMP ("agen-install-" + [guid]::NewGuid())
$env:AGEN_INSTALL_DIR = Join-Path $base "bin"; $env:AGEN_HOME = Join-Path $base "home"
$install = Join-Path $PSScriptRoot "install.ps1"
$registered = {
  @(Get-Service | Where-Object { $_.Name -like "*agen*" }).Count +
  @(Get-ScheduledTask -ErrorAction SilentlyContinue | Where-Object { $_.TaskName -like "*agen*" }).Count +
  @((Get-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -ErrorAction SilentlyContinue).PSObject.Properties |
    Where-Object { $_.Name -like "*agen*" }).Count
}
$registeredBefore = & $registered
try {
  # A wrong checksum is refused and installs nothing.
  $refused = $false
  try { & $install $Archive -Sha256 ("0" * 64) | Out-Null } catch { $refused = $true }
  if (-not $refused -or (Test-Path (Join-Path $env:AGEN_INSTALL_DIR "agen.exe"))) { throw "wrong checksum accepted" }

  & $install $Archive | Out-Null
  $agen = Join-Path $env:AGEN_INSTALL_DIR "agen.exe"
  $binDir = $env:AGEN_INSTALL_DIR
  $mine = { @(Get-Process agen-host,agen -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path.StartsWith($binDir) }).Count }
  $up = Start-Process -FilePath $agen -ArgumentList "up","--listen","127.0.0.1:7395","--gateway-listen","127.0.0.1:0" -PassThru -WindowStyle Hidden
  $deadline = (Get-Date).AddSeconds(30)
  while (-not (Test-Path (Join-Path $env:AGEN_HOME "local.json")) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 300 }
  Start-Sleep 2
  Push-Location $base
  try { & $agen init | Out-Null; & $agen deploy hello --replicas 2 | Out-Null } finally { Pop-Location }
  $out = & $agen run hello "hi"
  if ($out -ne "Hello! Nice to meet you.") { throw "run: $out" }
  Start-Sleep 3
  if ((& $mine) -lt 3) { throw "expected agen + 2 hosts running" }
  & $agen down | Out-Null
  Start-Sleep 2
  if ((& $mine) -ne 0 -or -not $up.HasExited) { throw "processes left after agen down" }
  if ((& $registered) -ne $registeredBefore) { throw "a service, scheduled task or Run key named agen appeared" }

  # -Purge refuses an AGEN_HOME that is not Agen's.
  $realHome = $env:AGEN_HOME
  $env:AGEN_HOME = Join-Path $base "not-agen"
  New-Item -ItemType Directory $env:AGEN_HOME | Out-Null
  Set-Content (Join-Path $env:AGEN_HOME "precious.txt") "keep me"
  $refused = $false
  try { & $install -Uninstall -Purge | Out-Null } catch { $refused = $true }
  if (-not $refused -or -not (Test-Path (Join-Path $env:AGEN_HOME "precious.txt"))) { throw "purge deleted a non-Agen directory" }
  $env:AGEN_HOME = $realHome

  & $install -Uninstall -Purge | Out-Null
  if ((Test-Path $env:AGEN_INSTALL_DIR) -or (Test-Path $env:AGEN_HOME)) { throw "files left after uninstall" }
  "install/up/down/uninstall OK"
} finally {
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $base
}
