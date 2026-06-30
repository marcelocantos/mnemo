# 🎯T100: Windows build+test of mnemo on the Parallels VM (windows/arm64,
# clang CGO). Driven by scripts/win-validate.sh; run on the VM via
# `powershell -File`. Mirrors .github/workflows/ci.yml's Windows job:
# check out the commit + sqldeep, build libsqldeep.a with clang, then
# `go test -tags sqlite_fts5`. Prereqs (provisioned once): Go, MSYS2
# CLANGARM64 toolchain (clang/llvm-ar) + mingw-w64-clang-aarch64-sqlite3.
param(
  [string]$Sha = "origin/master",
  [string]$SqldeepRef = "v0.22.0",
  [string]$Work = "C:\Users\marcelo\winci"
)

$ErrorActionPreference = "Continue"  # Go writes progress to stderr; don't treat as fatal.
$env:Path = "C:\msys64\clangarm64\bin;C:\Program Files\Go\bin;" + $env:Path
New-Item -ItemType Directory -Force -Path $Work | Out-Null
Set-Location $Work

# --- mnemo: the commit under test ---
if (-not (Test-Path mnemo\.git)) { git clone --quiet https://github.com/marcelocantos/mnemo.git mnemo }
Set-Location mnemo
git fetch --quiet origin
git checkout --quiet -f $Sha
"mnemo_head=$(git rev-parse --short HEAD)" | Write-Host
Set-Location ..

# --- sqldeep: pinned C/C++ dependency built into libsqldeep.a ---
if (-not (Test-Path sqldeep\.git)) { git clone --quiet --recurse-submodules https://github.com/marcelocantos/sqldeep.git sqldeep }
Set-Location sqldeep
git fetch --quiet --tags
git checkout --quiet -f $SqldeepRef
git submodule update --init --recursive --quiet

$CC = "clang"; $CXX = "clang++"; $AR = "llvm-ar"; $DP = "vendor/deepparser/src"
Remove-Item -Recurse -Force build -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path build\deepparser | Out-Null
$cflags = @("-Wall","-Wextra","-Wno-unused-parameter","-Wno-sign-compare","-O2","-DNDEBUG","-I$DP")
foreach ($f in "arena","liteparser","lp_tokenize","lp_unparse","parse") {
  & $CC @cflags -c "$DP/$f.c" -o "build/deepparser/$f.o"
  if ($LASTEXITCODE -ne 0) { "win_validate_fail=sqldeep_cc_$f" | Write-Host; exit 11 }
}
& $CXX @("-std=c++20","-Idist","-Ivendor/include","-I$DP") -c dist/sqldeep.cpp -o build/sqldeep.o
if ($LASTEXITCODE -ne 0) { "win_validate_fail=sqldeep_cpp" | Write-Host; exit 12 }
& $CC @("-w","-Idist","-Ivendor/include") -c dist/sqldeep_xml.c -o build/sqldeep_xml.o
if ($LASTEXITCODE -ne 0) { "win_validate_fail=sqldeep_xml" | Write-Host; exit 13 }
& $AR rcs build/libsqldeep.a build/sqldeep.o (Get-ChildItem build/deepparser/*.o | ForEach-Object { $_.FullName })
if (-not (Test-Path build/libsqldeep.a)) { "win_validate_fail=libsqldeep_missing" | Write-Host; exit 14 }
Set-Location ..

# --- go test (CGO sqlite + sqldeep) ---
Set-Location mnemo
$env:CGO_ENABLED = "1"
$env:CC = "clang"
$env:CXX = "clang++"  # sqlift's cgo compiles C++; Go defaults CXX to g++, absent in clangarm64
$env:CGO_CPPFLAGS = "-IC:/msys64/clangarm64/include"
go test -tags sqlite_fts5 -timeout=25m ./... 2>&1 | Tee-Object "$Work\gotest.log" | Out-Null
$code = $LASTEXITCODE
"go_test_exit=$code" | Write-Host
if ($code -ne 0) {
  "--- failing tail ---" | Write-Host
  Get-Content "$Work\gotest.log" -Tail 50 | Write-Host
}
exit $code
