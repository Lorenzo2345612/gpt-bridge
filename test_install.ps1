#Requires -Version 5.1
param([Parameter(Mandatory=$true)][string]$Binary)
$ErrorActionPreference = 'Stop'
$Root = Join-Path ([IO.Path]::GetTempPath()) ('gpt bridge tests ' + [guid]::NewGuid().ToString('N'))
$Installer = Join-Path $PSScriptRoot 'install.ps1'
$UserPathBefore = [Environment]::GetEnvironmentVariable('Path', 'User')
function Assert($Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
New-Item -ItemType Directory -Path $Root | Out-Null
try {
    $Prefix = Join-Path $Root 'local prefix'
    $TestProfile = Join-Path $Root 'profile.ps1'
    $Original = "function yolo { 'native-yolo' }`r`nfunction claude-2-yolo { 'native-secondary' }`r`n"
    [IO.File]::WriteAllText($TestProfile, $Original)
    & $Installer -Prefix $Prefix -Binary $Binary -Alias bridge-test -YoloAlias bridge-test-yolo -ProfilePath $TestProfile -NoPath
    . $TestProfile
    Assert ((bridge-test --bridge-version) -match '0\.3\.0') 'Normal alias failed'
    $env:CLAUDE_YOLO = 'previous'
    $env:CLAUDE_CODE_AUTO_COMPACT_WINDOW = '12345'
    Assert ((bridge-test-yolo --bridge-version) -match '0\.3\.0') 'YOLO alias failed'
    Assert ($env:CLAUDE_YOLO -eq 'previous' -and $env:CLAUDE_CODE_AUTO_COMPACT_WINDOW -eq '12345') 'YOLO wrapper did not restore environment'
    Remove-Item Env:CLAUDE_YOLO, Env:CLAUDE_CODE_AUTO_COMPACT_WINDOW
    $null = bridge-test-yolo --bridge-version
    Assert (-not (Test-Path Env:CLAUDE_YOLO)) 'YOLO environment leaked'
    Assert ((yolo) -eq 'native-yolo') 'Native yolo was replaced'
    Assert ((claude-2-yolo) -eq 'native-secondary') 'Secondary alias was replaced'
    & $Installer -Prefix $Prefix -Binary $Binary -Alias bridge-test -YoloAlias bridge-test-yolo -ProfilePath $TestProfile -NoPath
    $Text = [IO.File]::ReadAllText($TestProfile)
    Assert ($Text.StartsWith($Original)) 'Profile content changed'
    Assert (([regex]::Matches($Text, '# gpt-bridge aliases')).Count -eq 1) 'Duplicate profile source line'
    foreach ($Name in @('yolo', 'bad;command', 'same')) {
        $Rejected = $false
        try { & $Installer -Prefix $Prefix -Binary $Binary -Alias $Name -YoloAlias same -ProfilePath $TestProfile -NoPath } catch { $Rejected = $true }
        Assert $Rejected ('Invalid/conflicting alias accepted: ' + $Name)
        Assert ([IO.File]::ReadAllText($TestProfile) -eq $Text) 'Rejected installation changed profile'
    }
    & $Installer -Prefix (Join-Path $Root 'no aliases') -Binary $Binary -NoAliases -NoPath -ProfilePath (Join-Path $Root 'absent-profile.ps1')
    Assert (-not (Test-Path (Join-Path $Root 'absent-profile.ps1'))) 'NoAliases created a profile'
    Assert ([Environment]::GetEnvironmentVariable('Path', 'User') -eq $UserPathBefore) 'NoPath changed user PATH'
    Write-Host 'PASS: Windows installation, aliases, environment, conflicts and idempotence'
} finally {
    Remove-Item -LiteralPath $Root -Recurse -Force -ErrorAction SilentlyContinue
}
