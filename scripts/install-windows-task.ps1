#Requires -Version 5.1
<#
.SYNOPSIS
  Install, validate, or uninstall the DevAgent daemon Task Scheduler job
  (Windows 24/7 automation, FR-GO-14 / #203). The Windows counterpart of
  scripts/install-scout-launchagent.sh.

.DESCRIPTION
  Registers a per-user scheduled task that starts the DevAgent daemon at
  logon and restarts it on failure (KeepAlive equivalent). The task is
  registered for the current user only — no admin rights, no stored
  credentials, interactive token.

  %DEVAGENT_HOME% layout created/expected (default %USERPROFILE%\.devagent):

    %DEVAGENT_HOME%\bin\devagent.exe   the Go CLI (cmd/devagent build output)
    %DEVAGENT_HOME%\logs\daemon.log    appended stdout/stderr of the daemon
    %DEVAGENT_HOME%\daemon-token       written by the daemon at startup (0600-equivalent ACL)
    %DEVAGENT_HOME%\runs\              per-run JSONL logs

  Dry-run mode (-DryRun) prints the exact task action, trigger, and settings
  without touching the machine: the script is reviewed-by-construction and
  the CI has no Windows runner available to execute it (docs/WINDOWS.md).

.EXAMPLE
  .\install-windows-task.ps1 -RepoPath C:\src\devagent -DryRun
  .\install-windows-task.ps1 -RepoPath C:\src\devagent
  .\install-windows-task.ps1 -RepoPath C:\src\devagent -Validate
  .\install-windows-task.ps1 -TaskName DevAgentDaemon -Uninstall
#>
[CmdletBinding()]
param(
  # Repository the daemon serves. Required for install, ignored otherwise.
  [string]$RepoPath,

  # DevAgent home directory (paths differences: no /home, no $HOME/.devagent —
  # see docs/WINDOWS.md).
  [string]$DevagentHome = $(if ($env:DEVAGENT_HOME) { $env:DEVAGENT_HOME } else { Join-Path $env:USERPROFILE '.devagent' }),

  [string]$TaskName = 'DevAgentDaemon',

  # Print the registration that WOULD happen; execute nothing.
  [switch]$DryRun,

  # Parameter/syntax validation only (the plutil -lint analogue).
  [switch]$Validate,

  # Remove the task (and only the task — never touches %DEVAGENT_HOME%).
  [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'

$ExePath = Join-Path $DevagentHome 'bin\devagent.exe'
$LogPath = Join-Path $DevagentHome 'logs\daemon.log'

function Assert-Repo {
  if (-not $RepoPath) { throw "-RepoPath <path> is required (or use -Validate/-DryRun/-Uninstall)" }
  if (-not (Test-Path -LiteralPath $RepoPath -PathType Container)) { throw "repo path not found: $RepoPath" }
}

if ($Uninstall) {
  $existing = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  if (-not $existing) {
    Write-Output "task '$TaskName' not present; nothing to do"
    exit 0
  }
  if ($DryRun) { Write-Output "DRY-RUN: would Unregister-ScheduledTask -TaskName $TaskName"; exit 0 }
  Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
  Write-Output "uninstalled $TaskName"
  exit 0
}

Assert-Repo

# Read-back validation (the launchctl bootstrap + grep assertion analogue):
# after install, the task description must exist and its action must point at
# the exe we registered.
function Assert-Registered {
  $task = Get-ScheduledTask -TaskName $TaskName -ErrorAction Stop
  $exec = $task.Actions[0].Execute
  if ($exec -notlike '*devagent.exe*') { throw "registered action does not reference devagent.exe: $exec" }
}

if ($Validate) {
  if (-not (Test-Path -LiteralPath $ExePath)) { throw "missing $ExePath; build cmd/devagent first (go build -o bin\devagent.exe .\cmd\devagent)" }
  Write-Output "validate ok: $ExePath present; task '$TaskName' not inspected (use -DryRun to print its shape)"
  exit 0
}

# The action routes through cmd.exe so stdout/stderr append to the daemon log
# (Task Scheduler itself does not capture output; the LaunchAgent's
# StandardOutPath/StandardErrorPath analogue).
$inner = "`"$ExePath`" daemon --repo `"$RepoPath`" >> `"$LogPath`" 2>&1"
$action = New-ScheduledTaskAction -Execute "$env:ComSpec" -Argument "/c $inner" -WorkingDirectory $RepoPath

# At logon + KeepAlive analogue: restart up to 3 times per day, 1 minute
# apart; no execution-time limit (24/7 daemon); start as soon as possible
# after a missed logon slot.
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet `
  -RestartCount 3 `
  -RestartInterval (New-TimeSpan -Minutes 1) `
  -ExecutionTimeLimit ([TimeSpan]::Zero) `
  -StartWhenAvailable `
  -AllowStartIfOnBatteries `
  -DontStopIfGoingOnBatteries

if ($DryRun) {
  Write-Output "DRY-RUN: no changes made. Would register:"
  Write-Output "  TaskName : $TaskName"
  Write-Output "  Execute  : $env:ComSpec"
  Write-Output "  Argument : /c $inner"
  Write-Output "  WorkDir  : $RepoPath"
  Write-Output "  Trigger  : AtLogOn ($env:USERNAME)"
  Write-Output "  Settings : RestartCount=3 RestartInterval=1min ExecutionTimeLimit=0 StartWhenAvailable"
  Write-Output "  Exe      : $ExePath"
  Write-Output "  Log      : $LogPath"
  exit 0
}

if (-not (Test-Path -LiteralPath $ExePath)) { throw "missing $ExePath; build cmd/devagent first (go build -o bin\devagent.exe .\cmd\devagent)" }
New-Item -ItemType Directory -Force -Path (Join-Path $DevagentHome 'logs') | Out-Null

# Register for the current user with an interactive token: no admin rights
# and no password prompt. -Force overwrites a previous install of the same
# task name (same slot policy as the LaunchAgent label).
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Assert-Registered
Start-ScheduledTask -TaskName $TaskName

Write-Output "installed + started $TaskName (daemon --repo $RepoPath, log $LogPath)"
