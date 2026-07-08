# Fork install entry point — sets fork defaults and delegates to install.ps1.
if (-not $env:MULTICA_GITHUB_REPO) {
    $env:MULTICA_GITHUB_REPO = "Git-on-my-level/multica"
}
$env:MULTICA_SKIP_BREW = "1"
if (-not $env:MULTICA_CLI_REF) {
    $env:MULTICA_CLI_REF = "main"
}
if (-not $env:MULTICA_GITHUB_BRANCH) {
    $env:MULTICA_GITHUB_BRANCH = "main"
}

$repo = $env:MULTICA_GITHUB_REPO
$branch = $env:MULTICA_GITHUB_BRANCH

if ($PSScriptRoot -and (Test-Path (Join-Path $PSScriptRoot "install.ps1"))) {
    & (Join-Path $PSScriptRoot "install.ps1") @args
    exit $LASTEXITCODE
}

# irm .../install-fork.ps1 | iex — download install.ps1 from the fork repo.
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) "multica-install.ps1"
try {
    Invoke-WebRequest -Uri "https://raw.githubusercontent.com/$repo/$branch/scripts/install.ps1" -OutFile $tmp -UseBasicParsing
    & $tmp @args
    exit $LASTEXITCODE
} finally {
    if (Test-Path $tmp) {
        Remove-Item $tmp -Force -ErrorAction SilentlyContinue
    }
}
