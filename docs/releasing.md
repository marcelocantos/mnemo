# Releasing mnemo

Releases are cut locally from the macOS/arm64 release host.
There is no GitHub Actions workflow; `make release` builds, packages,
uploads and updates the Homebrew tap (via tapper).

## What gets built

| Artifact | Contents | Built on |
|---|---|---|
| `mnemo-VER-darwin-arm64.tar.gz` | `mnemo`, `NOTICE`, `LICENSE.zstd` | release host (native, Developer ID signed when available) |
| `mnemo-VER-windows-amd64.zip` | `mnemo.exe`, `NOTICE`, `LICENSE.zstd` | Windows VM (`hms-vm`), llvm-mingw cross |
| `mnemo-VER-windows-arm64.zip` | `mnemo.exe`, `NOTICE`, `LICENSE.zstd` | Windows VM, llvm-mingw |
| `mnemo-VER-windows-amd64-setup.exe` | Inno Setup installer | Windows VM |
| `mnemo-VER-windows-arm64-setup.exe` | Inno Setup installer | Windows VM |

Plus `SHA256SUMS`. Linux is not a release target.

Every binary is built from a fresh clone of the released commit next to
a pinned sqldeep (`SQLDEEP_REF`, default `v0.23.0`), never from a
working tree.

## Steps

1. Land the "Release prep: vX.Y.Z" commit on `master` (bumps `version`
   in `main.go`) through the normal local gates: `make bullseye` and
   `scripts/win-validate.sh` PASS. Releases are MINOR-only (`x.Y.0`).
2. Optionally run `make e2e`.
3. Dry run — builds and packages everything into `dist/vX.Y.Z/`, but
   tags, uploads and pushes nothing (tapper is not run):

   ```bash
   make release VERSION=X.Y.Z DRY_RUN=1
   ```

4. Real run — tags `vX.Y.Z` at `origin/master`, creates the GitHub
   release (or reuses one that already exists), uploads the assets with
   local `gh`, then updates `Formula/mnemo.rb` in
   `marcelocantos/homebrew-tap` with `tapper push` (needs Docker
   running and the tap token in the keychain; `tapper token status`):

   ```bash
   make release VERSION=X.Y.Z NOTES=/path/to/notes.md
   ```

   The real run refuses a dirty tree, a HEAD other than `origin/master`,
   a `main.go` version that doesn't match, or an existing tag on another
   commit.

## Signing

The darwin binary is signed with a Developer ID Application identity so
macOS TCC grants survive `brew upgrade` (🎯T101). `release.sh` uses
`MNEMO_SIGN_IDENTITY`, or else the first "Developer ID Application"
identity in the login keychain. With neither, the binary ships with the
linker's ad-hoc signature and the script prints a warning.

## Homebrew formula

[tapper](https://github.com/marcelocantos/tapper) renders and pushes the
formula from `tapper.yaml` (darwin/arm64 only; `depends_on` lines,
`service` block, install and test live there). Edit `tapper.yaml`, not
the tap copy. `make release` calls it via `scripts/release-tap.sh`
after the assets upload; to re-publish a release by hand, run
`make release-tap TAG=vX.Y.Z`.

## Windows VM

The Windows half is `scripts/win-release.ps1`, run on `hms-vm` over ssh
in `C:\Users\marcelo\winrel`. One-time toolchain setup (llvm-mingw
20250528 and Inno Setup 6.7.3, both under `winrel\tools`):

```bash
scripts/win-release.sh --provision
```

It also needs Go and MSYS2 clangarm64's `sqlite3.h`, which the
win-validate gate already provisions. If the VM is suspended, resume it
(`prlctl resume "Windows 11"`). The script waits (up to 30 min) if
another `go` or Inno Setup job is already running on the VM.
