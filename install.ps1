#Requires -Version 5.1
[CmdletBinding()]
param(
    [string]$Prefix = (Join-Path $env:LOCALAPPDATA 'gpt-bridge'),
    [string]$Binary,
    [Alias('Alias')][string]$NormalAlias,
    [string]$YoloAlias,
    [string]$ProfilePath = $PROFILE.CurrentUserAllHosts,
    [switch]$NoAliases,
    [switch]$NoPath
)
$ErrorActionPreference = 'Stop'
$SourceDirectory = $PSScriptRoot
$Prefix = [IO.Path]::GetFullPath($Prefix)
$BinDirectory = Join-Path $Prefix 'bin'
$Executable = Join-Path $BinDirectory 'claude-codex.exe'
$Marker = Join-Path $Prefix 'installation.json'
$AliasFile = Join-Path $Prefix 'aliases.ps1'
$ManagedHeader = '# Managed by gpt-bridge installer'
$Interactive = -not [Console]::IsInputRedirected
$Utf8 = New-Object Text.UTF8Encoding($true)

function Quote-PowerShell([string]$Value) { return "'" + $Value.Replace("'", "''") + "'" }
function Read-AliasName([string]$Label, [string]$Value, [string]$DefaultName, [bool]$Supplied) {
    while ($true) {
        if ([string]::IsNullOrEmpty($Value)) {
            if ($Interactive -and -not $Supplied) {
                $Value = Read-Host "$Label [$DefaultName] (- to skip)"
                if ([string]::IsNullOrWhiteSpace($Value)) { $Value = $DefaultName }
            } else { $Value = '-' }
        }
        if ($Value -eq '-') { return '-' }
        $Conflict = $null
        if ($Value -notmatch '^[a-zA-Z_][a-zA-Z0-9_-]*$') {
            $Conflict = 'Use letters, digits, underscores and hyphens; start with a letter or underscore.'
        } else {
            $Existing = Get-Command $Value -ErrorAction SilentlyContinue
            if ($Existing) {
                $Owned = ($Existing.CommandType -eq 'Alias' -and $Existing.Definition -eq $Executable) -or
                         ($Existing.CommandType -eq 'Function' -and $Existing.Definition.Contains('# gpt-bridge managed YOLO')) -or
                         ($Existing.CommandType -eq 'Application' -and $Existing.Source -eq $Executable)
                if (-not $Owned) { $Conflict = "'$Value' already exists and will be preserved; choose another name." }
            }
            if (-not $Conflict -and (Test-Path -LiteralPath $ProfilePath)) {
                $Pattern = '(?im)^\s*(?:function\s+(?:global:)?|Set-Alias\s+(?:-Name\s+)?[''"'']?)' + [regex]::Escape($Value) + '(?:\s|=|[''"'']|\{)'
                if ([IO.File]::ReadAllText($ProfilePath) -match $Pattern) {
                    $Conflict = "'$Value' is defined in your profile; choose another name."
                }
            }
        }
        if (-not $Conflict) { return $Value }
        if (-not $Interactive -or $Supplied) { throw $Conflict }
        Write-Host $Conflict
        $Value = ''
    }
}

if (-not $NoAliases) {
    $NormalSupplied = $PSBoundParameters.ContainsKey('NormalAlias')
    $YoloSupplied = $PSBoundParameters.ContainsKey('YoloAlias')
    if (-not $Interactive -and -not $NormalSupplied -and -not $YoloSupplied) {
        throw 'Run interactively or pass -NoAliases / -Alias NAME -YoloAlias NAME.'
    }
    $NormalAlias = Read-AliasName 'Normal alias' $NormalAlias 'gpt' $NormalSupplied
    $YoloAlias = Read-AliasName 'YOLO alias (skips permission prompts)' $YoloAlias 'gpt-yolo' $YoloSupplied
    if ($NormalAlias -ne '-' -and $NormalAlias -eq $YoloAlias) { throw 'Normal and YOLO aliases must differ.' }
}
if ((Test-Path -LiteralPath $Executable) -and -not (Test-Path -LiteralPath $Marker)) {
    throw "Refusing to replace an unrelated executable: $Executable"
}
if ((Test-Path -LiteralPath $AliasFile) -and (Get-Content -LiteralPath $AliasFile -TotalCount 1) -ne $ManagedHeader) {
    throw "Refusing to replace an unrelated alias file: $AliasFile"
}
if (-not $Binary) {
    $Go = $env:CLAUDE_CODEX_GO
    if (-not $Go) { $Go = 'go' }
    $Dist = Join-Path $SourceDirectory 'dist'
    New-Item -ItemType Directory -Force -Path $Dist | Out-Null
    $Binary = Join-Path $Dist 'claude-codex.exe'
    $HadCgo = Test-Path Env:CGO_ENABLED
    $PreviousCgo = $env:CGO_ENABLED
    try {
        $env:CGO_ENABLED = '0'
        & $Go -C $SourceDirectory build -trimpath '-ldflags=-s -w' -o $Binary .
        if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
    } finally {
        if ($HadCgo) { $env:CGO_ENABLED = $PreviousCgo } else { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue }
    }
}
$Binary = [IO.Path]::GetFullPath($Binary)
& $Binary --bridge-version
if ($LASTEXITCODE -ne 0) { throw 'The supplied executable cannot run on this Windows system.' }
New-Item -ItemType Directory -Force -Path $BinDirectory | Out-Null
$Temporary = Join-Path $BinDirectory ('.install-' + [guid]::NewGuid().ToString('N') + '.exe')
$Previous = $null
try {
    Copy-Item -LiteralPath $Binary -Destination $Temporary
    if (Test-Path -LiteralPath $Executable) {
        # Renaming permits updates when an older executable is still mapped.
        $Previous = $Executable + '.old-' + [guid]::NewGuid().ToString('N')
        Move-Item -LiteralPath $Executable -Destination $Previous
    }
    try { Move-Item -LiteralPath $Temporary -Destination $Executable }
    catch {
        if ($Previous -and -not (Test-Path -LiteralPath $Executable)) { Move-Item -LiteralPath $Previous -Destination $Executable }
        throw
    }
} finally {
    Remove-Item -LiteralPath $Temporary -ErrorAction SilentlyContinue
}
if ($Previous) { Remove-Item -LiteralPath $Previous -ErrorAction SilentlyContinue }
[IO.File]::WriteAllText($Marker, (@{ application='gpt-bridge'; executable=$Executable } | ConvertTo-Json), $Utf8)
Copy-Item -LiteralPath (Join-Path $SourceDirectory 'README.md') -Destination (Join-Path $Prefix 'README.md')

# The aliases use the absolute executable path; adding bin also exposes the
# original claude-codex command in future terminals without replacing PATH.
if (-not $NoPath) {
    $UserPath = [string][Environment]::GetEnvironmentVariable('Path', 'User')
    if (($UserPath -split ';') -notcontains $BinDirectory) {
        [Environment]::SetEnvironmentVariable('Path', (($UserPath.TrimEnd(';') + ';' + $BinDirectory).TrimStart(';')), 'User')
    }
    if (($env:Path -split ';') -notcontains $BinDirectory) { $env:Path += ';' + $BinDirectory }
}

if (-not $NoAliases -and ($NormalAlias -ne '-' -or $YoloAlias -ne '-')) {
    $Lines = @($ManagedHeader)
    if ($NormalAlias -ne '-') {
        $Lines += 'Set-Alias -Name ' + (Quote-PowerShell $NormalAlias) + ' -Value ' + (Quote-PowerShell $Executable) + ' -Scope Global'
    }
    if ($YoloAlias -ne '-') {
        $Function = @'
function global:__NAME__ {
    # gpt-bridge managed YOLO
    $hadYolo = Test-Path Env:CLAUDE_YOLO
    $hadWindow = Test-Path Env:CLAUDE_CODE_AUTO_COMPACT_WINDOW
    $oldYolo = $env:CLAUDE_YOLO
    $oldWindow = $env:CLAUDE_CODE_AUTO_COMPACT_WINDOW
    try {
        $env:CLAUDE_YOLO = '1'
        $env:CLAUDE_CODE_AUTO_COMPACT_WINDOW = '600000'
        & __EXECUTABLE__ --dangerously-skip-permissions @args
    } finally {
        if ($hadYolo) { $env:CLAUDE_YOLO = $oldYolo } else { Remove-Item Env:CLAUDE_YOLO -ErrorAction SilentlyContinue }
        if ($hadWindow) { $env:CLAUDE_CODE_AUTO_COMPACT_WINDOW = $oldWindow } else { Remove-Item Env:CLAUDE_CODE_AUTO_COMPACT_WINDOW -ErrorAction SilentlyContinue }
    }
}
'@
        $Lines += $Function.Replace('__NAME__', $YoloAlias).Replace('__EXECUTABLE__', (Quote-PowerShell $Executable))
    }
    [IO.File]::WriteAllText($AliasFile, ($Lines -join "`r`n") + "`r`n", $Utf8)
    $SourceLine = '. ' + (Quote-PowerShell $AliasFile)
    $ProfileDirectory = Split-Path -Parent $ProfilePath
    New-Item -ItemType Directory -Force -Path $ProfileDirectory | Out-Null
    $ProfileText = ''
    $ProfileEncoding = $Utf8
    if (Test-Path -LiteralPath $ProfilePath) {
        $Reader = New-Object IO.StreamReader($ProfilePath, $true)
        try { $ProfileText = $Reader.ReadToEnd(); $ProfileEncoding = $Reader.CurrentEncoding } finally { $Reader.Dispose() }
    }
    if (($ProfileText -split '\r?\n') -notcontains $SourceLine) {
        [IO.File]::WriteAllText($ProfilePath, $ProfileText + "`r`n# gpt-bridge aliases`r`n" + $SourceLine + "`r`n", $ProfileEncoding)
    }
    Write-Host "Aliases saved: normal=$NormalAlias, yolo=$YoloAlias"
    Write-Host ('Activate in this terminal: . ' + (Quote-PowerShell $ProfilePath))
} else { Write-Host 'Existing aliases were preserved; no PowerShell profile was changed.' }
Write-Host "Installed: $Executable"
Write-Host 'Claude sessions and profile settings were preserved.'
