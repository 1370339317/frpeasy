# frpeasy agent installer / upgrader for Windows (PowerShell 5.1+)
# Must run in an elevated prompt (Run as Administrator).
#
#   .\install.ps1 -Server http://PANEL:8080 -Token <install-token>
#   .\install.ps1 -Server http://PANEL:8080 -Token <token> -Foreground   (debug)
#   .\install.ps1 -Uninstall
#
# Re-running upgrades / repairs the install. The new binary is verified with
# --version before the running one is touched; if it fails to start, the
# previous binary is restored automatically.
#
# Notes: a scheduled task is used instead of a Windows service, because a plain
# console program does not implement the SCM protocol (sc create would fail 1053).
[CmdletBinding()]
param(
  [string]$Server = "",
  [string]$Token  = "",
  [switch]$Uninstall,
  [switch]$Foreground
)

$ErrorActionPreference = "Stop"
$TaskName = "frpeasy-agent"
$dir = Join-Path $env:ProgramData "frpeasy-agent"
$exe = Join-Path $dir "frpeasy-agent.exe"
$bak = Join-Path $dir "frpeasy-agent.exe.bak"

function Test-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  $pr = New-Object Security.Principal.WindowsPrincipal($id)
  return $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-AgentProcess {
  return @(Get-Process -Name "frpeasy-agent" -ErrorAction SilentlyContinue)
}

function Remove-AgentTask {
  $t = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  if ($t) {
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
  }
  # A stale registration can survive an interrupted install; clear it too.
  $stale = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  if ($stale) { Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue }
}

function Stop-AgentProcess {
  $p = Get-AgentProcess
  if (-not $p) { return $true }
  $p | Stop-Process -Force -ErrorAction SilentlyContinue
  for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Milliseconds 250
    if (-not (Get-AgentProcess)) { return $true }
  }
  # Last resort: taskkill with the tree flag, then report honestly.
  & taskkill /F /IM frpeasy-agent.exe /T 2>$null | Out-Null
  Start-Sleep -Milliseconds 500
  return (-not (Get-AgentProcess))
}

function Fail($msg) {
  Write-Host "[frpeasy] ERROR: $msg" -ForegroundColor Red
  exit 1
}

function Get-ExeVersion($path) {
  try {
    $out = & $path --version 2>&1
    if ($LASTEXITCODE -ne 0) { return $null }
    $line = [string]($out | Select-Object -First 1)
    if ($line -like "frpeasy-agent *") { return $line.Trim() }
    return $null
  } catch {
    return $null
  }
}

function Start-AgentTask($exePath, $argLine) {
  $act = New-ScheduledTaskAction -Execute $exePath -Argument $argLine -WorkingDirectory $dir
  $trg = New-ScheduledTaskTrigger -AtStartup
  $set = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit (New-TimeSpan -Seconds 0)
  $prn = New-ScheduledTaskPrincipal -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest
  Register-ScheduledTask -TaskName $TaskName -Action $act -Trigger $trg -Settings $set -Principal $prn -Force | Out-Null
  Start-ScheduledTask -TaskName $TaskName
}

if (-not (Test-Admin)) {
  Fail "administrator rights required. Right-click PowerShell and choose 'Run as Administrator'."
}

if ($Uninstall) {
  Remove-AgentTask
  [void](Stop-AgentProcess)
  if (Test-Path $dir) { Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue }
  Write-Host "[frpeasy] uninstalled (task, process and $dir removed)."
  exit 0
}

if ($Server -eq "" -or $Token -eq "") {
  Fail "-Server and -Token are required. Example: .\install.ps1 -Server http://1.2.3.4:8080 -Token abc123"
}
$Server = $Server.TrimEnd("/")

$arch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { $arch = "arm64" }

Write-Host "[frpeasy] panel  : $Server"
Write-Host "[frpeasy] target : windows/$arch"

New-Item -ItemType Directory -Force -Path $dir | Out-Null

# ---- 1. download to a temp name and verify it before touching the old install
$tmp = "$exe.new"
if (Test-Path $tmp) { Remove-Item $tmp -Force -ErrorAction SilentlyContinue }
$tryArch = @($arch)
if ($arch -eq "arm64") { $tryArch += "amd64" }
$newVer = $null
foreach ($a in $tryArch) {
  $url = "$Server/api/agent/bin/windows/$a"
  Write-Host "[frpeasy] downloading $url ..."
  try {
    Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing -TimeoutSec 300
    $sz = (Get-Item $tmp).Length
    if ($sz -lt 1000000) { throw "downloaded file is too small ($sz bytes)" }
    $newVer = Get-ExeVersion $tmp
    if (-not $newVer) { throw "downloaded file cannot run (wrong architecture or corrupt download)" }
    Write-Host "[frpeasy] verified : $newVer ($([math]::Round($sz/1MB,2)) MB, windows/$a)"
    break
  } catch {
    Write-Host "[frpeasy]   failed: $($_.Exception.Message)" -ForegroundColor Yellow
    $newVer = $null
    if (Test-Path $tmp) { Remove-Item $tmp -Force -ErrorAction SilentlyContinue }
  }
}
if (-not $newVer) {
  Fail "could not download a working agent binary (see the HTTP error above). Nothing was changed on this machine. Check: 1) the panel URL is reachable from here; 2) the panel has an update source configured (panel -> Settings -> version & update) - the panel fetches the agent binary from there on first request; 3) that the panel itself can reach the internet or its configured mirror."
}

$oldVer = $null
if (Test-Path $exe) { $oldVer = Get-ExeVersion $exe }

# ---- 2. foreground debug mode (no service, nothing registered)
$taskArgs = '--server ' + $Server + ' --token ' + $Token + ' --data "' + $dir + '"'
if ($Foreground) {
  [void](Remove-AgentTask)
  [void](Stop-AgentProcess)
  Move-Item -Force $tmp $exe
  Write-Host "[frpeasy] running in foreground (Ctrl+C to stop) ..."
  & $exe --server $Server --token $Token --data $dir
  exit $LASTEXITCODE
}

# ---- 3. stop the old instance, keep a rollback copy, swap in the new binary
[void](Remove-AgentTask)
if (-not (Stop-AgentProcess)) {
  Fail "could not stop the running agent process (still holding $exe). Reboot or kill frpeasy-agent.exe manually, then re-run."
}
if (Test-Path $bak) { Remove-Item $bak -Force -ErrorAction SilentlyContinue }
$hadOld = Test-Path $exe
if ($hadOld) {
  try { Move-Item -Force $exe $bak } catch { Fail "could not back up the old binary: $($_.Exception.Message)" }
}
try {
  Move-Item -Force $tmp $exe
} catch {
  if ($hadOld) { Move-Item -Force $bak $exe -ErrorAction SilentlyContinue }
  Fail "could not install the new binary: $($_.Exception.Message)"
}

# ---- 4. register the scheduled task and start it
Start-AgentTask $exe $taskArgs

$ok = $false
for ($i = 0; $i -lt 20; $i++) {
  Start-Sleep -Seconds 1
  if (Get-AgentProcess) { $ok = $true; break }
}

if ($ok) {
  if (Test-Path $bak) { Remove-Item $bak -Force -ErrorAction SilentlyContinue }
  $p = Get-AgentProcess
  Write-Host "[frpeasy] agent is running (PID $($p[0].Id)), starts automatically at boot."
  if ($oldVer -and $oldVer -ne $newVer) { Write-Host "[frpeasy] upgraded: $oldVer  ->  $newVer" }
  Write-Host "[frpeasy] done. Open the panel: this machine should show up as a client within a few seconds."
  Write-Host "[frpeasy] uninstall later with: .\install.ps1 -Uninstall"
  exit 0
}

# ---- 5. new binary did not come up: roll back instead of leaving a dead client
Write-Host "[frpeasy] WARNING: the new agent did not start. Rolling back ..." -ForegroundColor Yellow
[void](Remove-AgentTask)
[void](Stop-AgentProcess)
if (Test-Path $bak) {
  Move-Item -Force $bak $exe -ErrorAction SilentlyContinue
  $restoreVer = Get-ExeVersion $exe
  if ($restoreVer) {
    Start-AgentTask $exe $taskArgs
    Start-Sleep -Seconds 3
    if (Get-AgentProcess) {
      Write-Host "[frpeasy] rolled back to $restoreVer, the old agent is running again."
    } else {
      Write-Host "[frpeasy] rollback binary restored but did not start either. Check Task Scheduler library > $TaskName." -ForegroundColor Yellow
    }
  }
} else {
  Write-Host "[frpeasy] there was no previous installation to roll back to." -ForegroundColor Yellow
}
Write-Host "[frpeasy] check: Get-ScheduledTask -TaskName $TaskName | Get-ScheduledTaskInfo"
exit 1
