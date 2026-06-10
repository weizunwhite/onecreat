param(
    [string]$InstallerPath = "dist\onecreat-windows-amd64-installer.exe",
    [string]$InstallDir = "",
    [string]$SummaryPath = "dist\windows-native-smoke-summary.json",
    [int]$LaunchSeconds = 6,
    [switch]$SkipDesktopLaunch,
    [switch]$KeepInstalled
)

$ErrorActionPreference = "Stop"

$isWindowsRuntime = if (Get-Variable -Name IsWindows -ErrorAction SilentlyContinue) {
    $IsWindows
} else {
    $env:OS -eq "Windows_NT"
}

if (-not $isWindowsRuntime) {
    throw "windows-native-smoke.ps1 must run on Windows."
}

$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $repoRoot

function Resolve-RepoPath {
    param([string]$PathValue)
    if ([System.IO.Path]::IsPathRooted($PathValue)) {
        return $PathValue
    }
    return Join-Path $repoRoot $PathValue
}

function Add-Check {
    param(
        [string]$Name,
        [bool]$Passed,
        [string]$Detail = ""
    )
    $script:checks += [ordered]@{
        name = $Name
        passed = $Passed
        detail = $Detail
    }
    if (-not $Passed) {
        throw "$Name failed. $Detail"
    }
}

function Invoke-MCP {
    param(
        [string]$ExePath,
        [hashtable]$Request
    )
    $psi = [System.Diagnostics.ProcessStartInfo]::new()
    $psi.FileName = $ExePath
    $psi.WorkingDirectory = Split-Path -Parent $ExePath
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true

    $proc = [System.Diagnostics.Process]::Start($psi)
    $payload = $Request | ConvertTo-Json -Depth 20 -Compress
    $proc.StandardInput.WriteLine($payload)
    $proc.StandardInput.Close()

    $stdout = $proc.StandardOutput.ReadToEnd()
    $stderr = $proc.StandardError.ReadToEnd()
    if (-not $proc.WaitForExit(15000)) {
        $proc.Kill()
        throw "MCP process timed out. stderr=$stderr"
    }
    if ($proc.ExitCode -ne 0) {
        throw "MCP process exited $($proc.ExitCode). stderr=$stderr"
    }
    $line = ($stdout -split "`r?`n" | Where-Object { $_.Trim() -ne "" } | Select-Object -First 1)
    if (-not $line) {
        throw "MCP process produced no JSON-RPC response. stderr=$stderr"
    }
    return $line | ConvertFrom-Json
}

$script:checks = @()
$summary = [ordered]@{
    runDir = $repoRoot
    installer = $null
    installDir = $null
    checks = $script:checks
    desktopLaunch = $null
    uninstall = $null
}
$failed = $false
$desktopProc = $null

try {
    $installer = Resolve-RepoPath $InstallerPath
    $summary.installer = $installer
    Add-Check "installer exists" (Test-Path $installer) $installer

    if ([string]::IsNullOrWhiteSpace($InstallDir)) {
        $base = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { $env:TEMP }
        $InstallDir = Join-Path $base "onecreatNativeSmoke"
    }
    $InstallDir = Resolve-RepoPath $InstallDir
    $summary.installDir = $InstallDir

    if (Test-Path $InstallDir) {
        Remove-Item -Recurse -Force $InstallDir
    }

    $installArgs = @("/S", "/D=$InstallDir")
    $install = Start-Process -FilePath $installer -ArgumentList $installArgs -Wait -PassThru
    Add-Check "silent install exit code" ($install.ExitCode -eq 0) "exitCode=$($install.ExitCode)"

    $desktopExe = Join-Path $InstallDir "reasonix-desktop.exe"
    $hardwareExe = Join-Path $InstallDir "reasonix-hardware-mcp.exe"
    $uninstallExe = Join-Path $InstallDir "uninstall.exe"
    Add-Check "desktop executable installed" (Test-Path $desktopExe) $desktopExe
    Add-Check "hardware MCP executable installed" (Test-Path $hardwareExe) $hardwareExe
    Add-Check "uninstaller installed" (Test-Path $uninstallExe) $uninstallExe

    $toolsResponse = Invoke-MCP $hardwareExe @{
        jsonrpc = "2.0"
        id = 1
        method = "tools/list"
        params = @{}
    }
    $toolNames = @($toolsResponse.result.tools | ForEach-Object { $_.name })
    Add-Check "hardware MCP tools/list includes evidence status" ($toolNames -contains "hardware_evidence_status") ($toolNames -join ",")

    $detectResponse = Invoke-MCP $hardwareExe @{
        jsonrpc = "2.0"
        id = 2
        method = "tools/call"
        params = @{
            name = "hardware_detect"
            arguments = @{
                project_dir = $InstallDir
            }
        }
    }
    Add-Check "hardware MCP hardware_detect returns content" ($detectResponse.result.content.Count -gt 0) ""

    if ($SkipDesktopLaunch) {
        $summary.desktopLaunch = [ordered]@{
            skipped = $true
            reason = "SkipDesktopLaunch was set."
        }
    } else {
        $desktopProc = Start-Process -FilePath $desktopExe -PassThru
        Start-Sleep -Seconds $LaunchSeconds
        $alive = -not $desktopProc.HasExited
        $summary.desktopLaunch = [ordered]@{
            skipped = $false
            processId = $desktopProc.Id
            aliveAfterSeconds = $LaunchSeconds
            passed = $alive
        }
        Add-Check "desktop process stays alive briefly" $alive "processId=$($desktopProc.Id)"
    }

    if ($desktopProc -and -not $desktopProc.HasExited) {
        Stop-Process -Id $desktopProc.Id -Force
        $desktopProc = $null
    }

    if ($KeepInstalled) {
        $summary.uninstall = [ordered]@{
            skipped = $true
            reason = "KeepInstalled was set."
        }
    } else {
        $uninstall = Start-Process -FilePath $uninstallExe -ArgumentList "/S" -Wait -PassThru
        Start-Sleep -Seconds 2
        $removed = -not (Test-Path $desktopExe) -and -not (Test-Path $hardwareExe)
        $summary.uninstall = [ordered]@{
            skipped = $false
            exitCode = $uninstall.ExitCode
            payloadRemoved = $removed
        }
        Add-Check "silent uninstall exit code" ($uninstall.ExitCode -eq 0) "exitCode=$($uninstall.ExitCode)"
        Add-Check "payload removed after uninstall" $removed $InstallDir
    }
} catch {
    $failed = $true
    $summary.error = $_.Exception.Message
    Write-Error $_
} finally {
    if ($desktopProc -and -not $desktopProc.HasExited) {
        Stop-Process -Id $desktopProc.Id -Force
    }
    $summary.checks = $script:checks
    $summaryPathAbs = Resolve-RepoPath $SummaryPath
    $summaryDir = Split-Path -Parent $summaryPathAbs
    if (-not (Test-Path $summaryDir)) {
        New-Item -ItemType Directory -Force -Path $summaryDir | Out-Null
    }
    $summary | ConvertTo-Json -Depth 20 | Set-Content -Path $summaryPathAbs -Encoding UTF8
    Write-Host "summary: $summaryPathAbs"
}

if ($failed) {
    exit 1
}
