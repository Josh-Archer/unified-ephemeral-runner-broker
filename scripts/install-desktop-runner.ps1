param(
    [Parameter(Mandatory=$true)]
    [string]$RepoUrl,
    
    [Parameter(Mandatory=$true)]
    [string]$Token,

    [string]$RunnerName = $env:COMPUTERNAME,
    [string]$InstallDir = "C:\actions-runner"
)

$ErrorActionPreference = 'Stop'

# Create installation directory
Write-Host "Creating installation directory at $InstallDir..."
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
}
Set-Location $InstallDir

# Download Runner
Write-Host "Downloading GitHub Actions Runner..."
Invoke-WebRequest -Uri "https://github.com/actions/runner/releases/download/v2.317.0/actions-runner-win-x64-2.317.0.zip" -OutFile "runner.zip"

# Extract Runner
Write-Host "Extracting Runner..."
Expand-Archive "runner.zip" -DestinationPath . -Force
Remove-Item "runner.zip"

# Configure Runner
Write-Host "Configuring Runner with UECB desktop labels..."
.\config.cmd --unattended --url $RepoUrl --token $Token --name $RunnerName --labels "desktop-runner,windows,desktop" --replace

# Setup Auto-Recovery on Boot (Startup Folder)
Write-Host "Setting up auto-recovery (Startup folder)..."
$startupFolder = "$env:APPDATA\Microsoft\Windows\Start Menu\Programs\Startup"
$vbsPath = "$startupFolder\GitHubActionsRunner.vbs"
$vbsContent = "Set WshShell = CreateObject(`"WScript.Shell`")`r`nWshShell.Run `"$InstallDir\run.cmd`", 0, False"
Set-Content -Path $vbsPath -Value $vbsContent

Write-Host "Installation Complete! To start it right now, run: Start-Process -FilePath `"$InstallDir\run.cmd`" -WindowStyle Hidden" -ForegroundColor Green
