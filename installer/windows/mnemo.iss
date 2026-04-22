; Inno Setup script for mnemo.
;
; Produces a double-click .exe installer that drops mnemo.exe into
; Program Files, registers it as a Windows Service (auto-start,
; restart-on-failure), and patches the invoking user's ~/.claude.json
; so Claude Code picks up mnemo on next session restart.
;
; Build:
;   iscc /DAppVersion=0.21.0 /DSourceDir=..\..\dist mnemo.iss
;
; AppVersion  — version string shown in Add/Remove Programs
;               and in the installer window title.
; SourceDir   — directory containing the freshly-built mnemo.exe
;               (the CI uses the same dir it extracts the zip into).
;
; The installer:
;   - Requires admin elevation (mandatory for service install +
;     writes to Program Files).
;   - Copies mnemo.exe into {app} (default C:\Program Files\mnemo).
;   - Runs `mnemo.exe install-service`  (elevated).
;   - Runs `mnemo.exe register-mcp`     (as original user, so the
;     right ~/.claude.json is patched — not the admin account's).
;   - Starts the service so the user doesn't have to reboot.
;
; The uninstaller:
;   - Runs `mnemo.exe unregister-mcp`   (as original user).
;   - Runs `mnemo.exe uninstall-service` (elevated).
;   - Removes {app} and the %ProgramData%\mnemo tree.

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef SourceDir
  #define SourceDir "."
#endif
#ifndef Arch
  #define Arch "amd64"
#endif

; Inno Setup architecture keywords differ from the Go GOARCH values
; we use everywhere else — translate once here so the rest of the
; [Setup] block stays readable.
#if Arch == "arm64"
  #define InnoArch "arm64"
#else
  #define InnoArch "x64compatible"
#endif

[Setup]
AppId={{C7F3B2A1-8E4D-4B5C-9A2F-1D6E8C7B9A0F}
AppName=mnemo
AppVersion={#AppVersion}
AppVerName=mnemo {#AppVersion}
AppPublisher=Marcelo Cantos
AppPublisherURL=https://github.com/marcelocantos/mnemo
AppSupportURL=https://github.com/marcelocantos/mnemo/issues
AppUpdatesURL=https://github.com/marcelocantos/mnemo/releases
DefaultDirName={autopf}\mnemo
DefaultGroupName=mnemo
DisableProgramGroupPage=yes
OutputBaseFilename=mnemo-{#AppVersion}-windows-{#Arch}-setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
PrivilegesRequired=admin
ArchitecturesInstallIn64BitMode={#InnoArch}
ArchitecturesAllowed={#InnoArch}
; Allow `uninstall-agent` and `unregister-mcp` to find mnemo.exe via
; full path even if PATH is not updated.
UninstallDisplayIcon={app}\mnemo.exe
UninstallDisplayName=mnemo {#AppVersion}
LicenseFile={#SourceDir}\LICENSE.txt

[Files]
Source: "{#SourceDir}\mnemo.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceDir}\LICENSE.txt"; DestDir: "{app}"; Flags: ignoreversion

[Run]
; Register the per-user Scheduled Task that runs mnemo at logon.
; `runasoriginaluser` is critical: the task must be registered for
; the real user's account (not the installer's elevated admin
; context), otherwise the task would run in the wrong user profile
; and os.UserHomeDir() would point at the wrong ~/.claude/projects/.
; install-agent also cleans up any v0.22.0-era Windows Service of
; the same name, so upgrading in place works.
Filename: "{app}\mnemo.exe"; Parameters: "install-agent"; \
  StatusMsg: "Registering mnemo user agent..."; \
  Flags: runhidden waituntilterminated runasoriginaluser

; MCP registration (also as the original user, so the right
; ~/.claude.json is patched).
Filename: "{app}\mnemo.exe"; Parameters: "register-mcp"; \
  StatusMsg: "Registering mnemo with Claude Code..."; \
  Flags: runhidden waituntilterminated runasoriginaluser

[UninstallRun]
; Remove the Scheduled Task (and any v0.22.0-era Service of the same
; name). We deliberately DO NOT invoke `mnemo unregister-mcp` at
; uninstall: Inno Setup's [UninstallRun] section does not support
; the `runasoriginaluser` flag, so the command would run as the
; elevated uninstaller account and patch the wrong ~/.claude.json.
; The stale entry in the real user's config is harmless — Claude
; Code will fail to connect and move on. Users who want a clean
; config can run `mnemo unregister-mcp` themselves before
; uninstalling, or delete the mnemo entry by hand afterwards.
Filename: "{app}\mnemo.exe"; Parameters: "uninstall-agent"; \
  Flags: runhidden waituntilterminated; \
  RunOnceId: "MnemoUninstallService"

[UninstallDelete]
; Drop the %ProgramData%\mnemo\ tree (logs, runtime state). The user's
; home-directory state (~/.mnemo/mnemo.db, ~/.claude/projects/) is
; deliberately preserved so reinstalling does not destroy the index.
Type: filesandordirs; Name: "{commonappdata}\mnemo"

