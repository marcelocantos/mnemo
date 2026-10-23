# Windows half of scripts/release.sh: build mnemo.exe for windows/amd64
# and windows/arm64 with llvm-mingw (clang, static CGO), then package the
# release zips and Inno Setup installers. Runs on the Parallels VM
# (hms-vm, Win11 ARM64) via `powershell -File`; release.sh scp's the
# results back from $Work\out.
#
# Toolchain lives under $Work\tools and is installed once with
# -Provision (llvm-mingw ucrt-aarch64 host build, Inno Setup per-user).
# Also needs Go and MSYS2 clangarm64's sqlite3 headers, which the
# win-validate gate already provisions.
param(
  [string]$Sha = "origin/master",
  [string]$Version = "0.0.0",
  [string]$SqldeepRef = "v0.23.0",
  [string]$Archs = "amd64,arm64",
  [string]$Work = "C:\Users\marcelo\winrel",
  [switch]$Provision,
  [switch]$ProvisionOnly
)

$ErrorActionPreference = "Continue"  # Go/git write progress to stderr.
$ProgressPreference = "SilentlyContinue"
$LlvmVer = "20250528"
$InnoVer = "6.7.3"
$Tools = "$Work\tools"
$Llvm = "$Tools\llvm-mingw-$LlvmVer-ucrt-aarch64\bin"
$Iscc = "$Tools\innosetup\ISCC.exe"
$Out = "$Work\out"
function Fail([string]$why, [int]$code) { "win_release_fail=$why" | Write-Host; "win_release_exit=$code" | Write-Host; exit $code }

New-Item -ItemType Directory -Force -Path $Work, $Tools | Out-Null

# Don't overlap another job's go build/test or installer build (the
# win-validate gate shares this VM). Wait up to 30 minutes.
for ($i = 0; $i -lt 180; $i++) {
  $busy = Get-Process -Name go, go1*, iscc, iscmplr -ErrorAction SilentlyContinue
  if (-not $busy) { break }
  if ($i -eq 0) { "waiting: VM busy ($(($busy | ForEach-Object Name) -join ','))" | Write-Host }
  Start-Sleep -Seconds 10
}
if ($busy) { Fail "vm_busy" 20 }

if ($Provision) {
  if (-not (Test-Path $Llvm)) {
    "provision: llvm-mingw $LlvmVer" | Write-Host
    $zip = "$Tools\llvm-mingw.zip"
    Invoke-WebRequest "https://github.com/mstorsjo/llvm-mingw/releases/download/$LlvmVer/llvm-mingw-$LlvmVer-ucrt-aarch64.zip" -OutFile $zip
    tar.exe -xf $zip -C $Tools
    Remove-Item $zip
  }
  if (-not (Test-Path $Iscc)) {
    "provision: Inno Setup $InnoVer" | Write-Host
    $exe = "$Tools\innosetup-$InnoVer.exe"
    Invoke-WebRequest "https://github.com/jrsoftware/issrc/releases/download/is-$($InnoVer -replace '\.','_')/innosetup-$InnoVer.exe" -OutFile $exe
    Start-Process -Wait -FilePath $exe -ArgumentList @("/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/CURRENTUSER", "/NOICONS", "/DIR=$Tools\innosetup")
    Remove-Item $exe
  }
}
if (-not (Test-Path "$Llvm\aarch64-w64-mingw32-clang.exe")) { Fail "llvm_mingw_missing (rerun with -Provision)" 2 }
if (-not (Test-Path $Iscc)) { Fail "iscc_missing (rerun with -Provision)" 2 }
if ($ProvisionOnly) { "tools_ok llvm=$Llvm iscc=$Iscc" | Write-Host; "win_release_exit=0" | Write-Host; exit 0 }

$env:Path = "$Llvm;C:\Program Files\Go\bin;C:\Program Files\Git\cmd;" + $env:Path
$env:GOWORK = "off"

# sqlite3.h only (architecture-independent); isolated so the MSYS2
# include tree can't shadow llvm-mingw's own CRT headers.
$Inc = "$Work\include"
New-Item -ItemType Directory -Force -Path $Inc | Out-Null
Copy-Item C:\msys64\clangarm64\include\sqlite3.h, C:\msys64\clangarm64\include\sqlite3ext.h $Inc -Force
if (-not (Test-Path "$Inc\sqlite3.h")) { Fail "sqlite3_header_missing" 3 }

Set-Location $Work
if (-not (Test-Path mnemo\.git)) { git clone --quiet https://github.com/marcelocantos/mnemo.git mnemo }
Set-Location mnemo
git fetch --quiet origin
git checkout --quiet -f $Sha
git clean -qfdx
"mnemo_head=$(git rev-parse --short HEAD)" | Write-Host
Set-Location $Work

if (-not (Test-Path sqldeep\.git)) { git clone --quiet --recurse-submodules https://github.com/marcelocantos/sqldeep.git sqldeep }
Set-Location sqldeep
git fetch --quiet --tags
git checkout --quiet -f $SqldeepRef
git submodule update --init --recursive --quiet
"sqldeep_ref=$SqldeepRef" | Write-Host
Set-Location $Work

Remove-Item -Recurse -Force $Out -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $Out | Out-Null

foreach ($arch in $Archs.Split(",")) {
  $triple = @{ amd64 = "x86_64"; arm64 = "aarch64" }[$arch]
  if (-not $triple) { Fail "unknown_arch_$arch" 4 }
  $CC = "$triple-w64-mingw32-clang"; $CXX = "$triple-w64-mingw32-clang++"; $AR = "llvm-ar"

  # libsqldeep.a for this target (build/ is shared, so rebuild per arch).
  Set-Location "$Work\sqldeep"
  $DP = "vendor/deepparser/src"
  Remove-Item -Recurse -Force build -ErrorAction SilentlyContinue
  New-Item -ItemType Directory -Force -Path build\deepparser | Out-Null
  $cflags = @("-Wall","-Wextra","-Wno-unused-parameter","-Wno-sign-compare","-O2","-DNDEBUG","-I$DP")
  foreach ($f in "arena","liteparser","lp_tokenize","lp_unparse","parse") {
    & $CC @cflags -c "$DP/$f.c" -o "build/deepparser/$f.o"
    if ($LASTEXITCODE -ne 0) { Fail "sqldeep_cc_${f}_$arch" 11 }
  }
  & $CXX @("-std=c++20","-O2","-Idist","-Ivendor/include","-I$DP") -c dist/sqldeep.cpp -o build/sqldeep.o
  if ($LASTEXITCODE -ne 0) { Fail "sqldeep_cpp_$arch" 12 }
  & $CC @("-w","-Idist","-Ivendor/include") -c dist/sqldeep_xml.c -o build/sqldeep_xml.o
  if ($LASTEXITCODE -ne 0) { Fail "sqldeep_xml_$arch" 13 }
  & $AR rcs build/libsqldeep.a build/sqldeep.o (Get-ChildItem build/deepparser/*.o | ForEach-Object { $_.FullName })
  if (-not (Test-Path build/libsqldeep.a)) { Fail "libsqldeep_missing_$arch" 14 }

  # mnemo.exe: GUI subsystem (no console flash when launched by the
  # service manager) and fully static so it needs no MinGW runtime DLLs.
  Set-Location "$Work\mnemo"
  $env:CGO_ENABLED = "1"; $env:GOOS = "windows"; $env:GOARCH = $arch
  $env:CC = $CC; $env:CXX = $CXX; $env:AR = $AR
  $env:CGO_CPPFLAGS = "-I$($Inc -replace '\\','/')"
  Remove-Item -Force mnemo.exe -ErrorAction SilentlyContinue
  go build -trimpath -tags sqlite_fts5 -ldflags "-s -w -H=windowsgui -extldflags=-static" -o mnemo.exe . 2>&1 | Write-Host
  if ($LASTEXITCODE -ne 0 -or -not (Test-Path mnemo.exe)) { Fail "go_build_$arch" 21 }
  Remove-Item Env:GOOS, Env:GOARCH

  $base = "mnemo-$Version-windows-$arch"
  $pkg = "$Work\pkg-$arch"
  Remove-Item -Recurse -Force $pkg -ErrorAction SilentlyContinue
  New-Item -ItemType Directory -Force -Path $pkg | Out-Null
  Copy-Item mnemo.exe, NOTICE $pkg
  Compress-Archive -Path "$pkg\mnemo.exe", "$pkg\NOTICE" -DestinationPath "$Out\$base.zip" -Force
  if (-not (Test-Path "$Out\$base.zip")) { Fail "zip_$arch" 22 }

  # Inno Setup resolves Source: paths relative to the .iss, so stage there.
  $stage = "installer\windows\dist"
  Remove-Item -Recurse -Force $stage -ErrorAction SilentlyContinue
  New-Item -ItemType Directory -Force -Path $stage | Out-Null
  Copy-Item mnemo.exe "$stage\mnemo.exe"
  Copy-Item LICENSE "$stage\LICENSE.txt"
  Copy-Item NOTICE "$stage\NOTICE.txt"
  & $Iscc /Q "/DAppVersion=$Version" "/DSourceDir=dist" "/DArch=$arch" "/O$Out" installer\windows\mnemo.iss
  if ($LASTEXITCODE -ne 0 -or -not (Test-Path "$Out\$base-setup.exe")) { Fail "iscc_$arch" 23 }
  "built $base (.zip, -setup.exe)" | Write-Host
}

Get-ChildItem $Out | ForEach-Object { "artifact=$($_.Name) size=$($_.Length)" | Write-Host }
"win_release_exit=0" | Write-Host
exit 0
