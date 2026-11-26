# OpenMail Windows CLI installer: irm https://github.com/michael-berardi/openmail/releases/latest/download/install.ps1 | iex
# Verifies SHA256SUMS, installs the CLI into %LOCALAPPDATA%\OpenMail\<version> and adds an openmail.cmd shim in
# %USERPROFILE%\.local\bin. Env overrides: OPENMAIL_VERSION (pin), OPENMAIL_RELEASE_BASE (local/mirror base URL),
# OPENMAIL_INSTALL_DIR and OPENMAIL_BIN_DIR. Existing files are replaced only when this installer created them.
$ErrorActionPreference = 'Stop'

function Resolve-LocalPath([string]$path) {
  if ([IO.Path]::IsPathRooted($path)) { return [IO.Path]::GetFullPath($path) }
  return [IO.Path]::GetFullPath((Join-Path (Get-Location).Path $path))
}

$versionOverride = $null
if ($env:OPENMAIL_VERSION) { $versionOverride = $env:OPENMAIL_VERSION.TrimStart('v') }
$base = if ($env:OPENMAIL_RELEASE_BASE) {
  $env:OPENMAIL_RELEASE_BASE.TrimEnd('/')
} elseif ($versionOverride) {
  "https://github.com/michael-berardi/openmail/releases/download/v$versionOverride"
} else {
  'https://github.com/michael-berardi/openmail/releases/latest/download'
}
$defaultRoot = if ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA 'OpenMail' } else { Join-Path $env:USERPROFILE '.local\share\openmail' }
$installRoot = Resolve-LocalPath $(if ($env:OPENMAIL_INSTALL_DIR) { $env:OPENMAIL_INSTALL_DIR } else { $defaultRoot })
$binDir = Resolve-LocalPath $(if ($env:OPENMAIL_BIN_DIR) { $env:OPENMAIL_BIN_DIR } else { Join-Path $env:USERPROFILE '.local\bin' })
if ($env:PROCESSOR_ARCHITECTURE -and $env:PROCESSOR_ARCHITECTURE -ne 'AMD64') {
  throw "No OpenMail Windows CLI asset is published for $env:PROCESSOR_ARCHITECTURE."
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) ('openmail-install-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
$backup = $null
try {
  $sums = Join-Path $tmp 'SHA256SUMS'
  Invoke-WebRequest "$base/SHA256SUMS" -OutFile $sums -UseBasicParsing
  $assetPattern = '^\s*([0-9a-fA-F]{64})\s+\*?(openmail-cli-v(\d+\.\d+\.\d+)-windows-amd64\.zip)\s*$'
  $line = @(Get-Content -LiteralPath $sums | Where-Object { $_ -match $assetPattern } | Select-Object -First 1)[0]
  if (-not $line -or $line -notmatch $assetPattern) { throw 'No Windows CLI asset in SHA256SUMS.' }
  $want = $Matches[1].ToLowerInvariant()
  $asset = $Matches[2]
  $version = $Matches[3]
  if ($versionOverride -and $version -ne $versionOverride) {
    throw "Release $version does not match the requested version $versionOverride."
  }

  $target = Join-Path $installRoot $version
  $marker = Join-Path $target '.openmail-installed'
  $shim = Join-Path $binDir 'openmail.cmd'
  if ((Test-Path -LiteralPath $target) -and -not (Test-Path -LiteralPath $marker)) {
    throw "Refusing to replace $target: this installer did not create it."
  }
  if (Test-Path -LiteralPath $marker) {
    $owner = ((Get-Content -LiteralPath $marker -TotalCount 1) | Out-String).Trim()
    if ($owner -ne 'OPENMAIL_INSTALLER_V1') { throw "Refusing to replace $target: invalid install marker." }
  }
  if (Test-Path -LiteralPath $shim) {
    $isOurs = @(Get-Content -LiteralPath $shim -ErrorAction SilentlyContinue) -contains 'REM openmail-installer-shim'
    if (-not $isOurs) { throw "Refusing to replace $shim: this installer did not create it." }
  }

  Write-Host "Downloading $asset..."
  $archive = Join-Path $tmp $asset
  Invoke-WebRequest "$base/$asset" -OutFile $archive -UseBasicParsing
  $got = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($got -ne $want) { throw "Checksum mismatch for $asset; nothing was installed." }

  $extract = Join-Path $tmp 'cli'
  New-Item -ItemType Directory -Path $extract | Out-Null
  Expand-Archive -LiteralPath $archive -DestinationPath $extract
  $exe = Get-ChildItem -LiteralPath $extract -Recurse -File -Filter 'openmail.exe' | Select-Object -First 1
  if (-not $exe) { throw 'The CLI archive does not contain openmail.exe.' }

  New-Item -ItemType Directory -Path $installRoot -Force | Out-Null
  New-Item -ItemType Directory -Path $binDir -Force | Out-Null
  $stage = Join-Path $installRoot ('.openmail-install-' + [guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $stage | Out-Null
  Copy-Item -LiteralPath $exe.FullName -Destination (Join-Path $stage 'openmail.exe')
  foreach ($docName in @('LICENSE', 'README.md')) {
    $doc = Get-ChildItem -LiteralPath $extract -Recurse -File -Filter $docName | Select-Object -First 1
    if ($doc) { Copy-Item -LiteralPath $doc.FullName -Destination (Join-Path $stage $docName) }
  }
  Set-Content -LiteralPath (Join-Path $stage '.openmail-installed') -Value 'OPENMAIL_INSTALLER_V1' -Encoding ASCII
  $stagedExe = Join-Path $stage 'openmail.exe'
  $out = (& $stagedExe version 2>&1 | Out-String)
  if ($LASTEXITCODE -ne 0 -or ($out -notlike "openmail $version*" -and $out -notlike "openmail v$version*")) {
    throw "The downloaded CLI does not report version $version`: $out"
  }

  if (Test-Path -LiteralPath $target) {
    $backup = Join-Path $installRoot ('.openmail-old-' + [guid]::NewGuid().ToString('N'))
    Move-Item -LiteralPath $target -Destination $backup
  }
  try {
    Move-Item -LiteralPath $stage -Destination $target
  } catch {
    if ($backup) { Move-Item -LiteralPath $backup -Destination $target }
    throw
  }
  $stage = $null
  if ($backup) { Remove-Item -LiteralPath $backup -Recurse -Force; $backup = $null }

  $shimTmp = Join-Path $binDir ('.openmail-shim-' + [guid]::NewGuid().ToString('N') + '.cmd')
  @('@echo off', 'REM openmail-installer-shim', '"' + (Join-Path $target 'openmail.exe') + '" %*', 'exit /b %ERRORLEVEL%') |
    Set-Content -LiteralPath $shimTmp -Encoding ASCII
  Move-Item -LiteralPath $shimTmp -Destination $shim -Force
  $out = (& $shim version 2>&1 | Out-String)
  if ($LASTEXITCODE -ne 0 -or ($out -notlike "openmail $version*" -and $out -notlike "openmail v$version*")) {
    throw "The installed shim does not report version $version`: $out"
  }

  Write-Host "OpenMail $version installed: $target\openmail.exe (shim: $shim)"
  if (($env:PATH -split ';') -notcontains $binDir) { Write-Host "Add $binDir to your PATH to use: openmail" }
} finally {
  if ($backup) { Remove-Item -LiteralPath $backup -Recurse -Force -ErrorAction SilentlyContinue }
  Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
