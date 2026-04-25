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
; Install the mnemo Windows Service (elevated — default installer
; context). install-service also tears down any legacy v0.23/v0.24
; Scheduled Task of the same name so upgrades are clean.
Filename: "{app}\mnemo.exe"; Parameters: "install-service"; \
  StatusMsg: "Installing mnemo Windows Service..."; \
  Flags: runhidden waituntilterminated

; Start the service immediately so the user does not need to
; reboot. Tolerate failure (already-running on upgrade).
Filename: "{sys}\net.exe"; Parameters: "start mnemo"; \
  StatusMsg: "Starting mnemo service..."; \
  Flags: runhidden waituntilterminated

; MCP registration (as the original user so the right
; ~/.claude.json is patched). The default URL embeds
; ?user=<current-user> so the daemon's per-user Registry resolves
; to the right home — critical because the service itself runs as
; LocalSystem and has no user identity of its own.
Filename: "{app}\mnemo.exe"; Parameters: "register-mcp"; \
  StatusMsg: "Registering mnemo with Claude Code..."; \
  Flags: runhidden waituntilterminated runasoriginaluser

[UninstallRun]
; Stop and remove the Windows Service (also tears down any legacy
; Scheduled Task). We deliberately DO NOT invoke `mnemo unregister-mcp`
; at uninstall: Inno Setup's [UninstallRun] section does not support
; the `runasoriginaluser` flag, so the command would run as the
; elevated uninstaller account and patch the wrong ~/.claude.json.
; The stale entry in the real user's config is harmless — Claude
; Code will fail to connect and move on. Users who want a clean
; config can run `mnemo unregister-mcp` themselves before
; uninstalling.
Filename: "{app}\mnemo.exe"; Parameters: "uninstall-service"; \
  Flags: runhidden waituntilterminated; \
  RunOnceId: "MnemoUninstallService"

[UninstallDelete]
; Drop the %ProgramData%\mnemo\ tree (logs, runtime state). The user's
; home-directory state (~/.mnemo/mnemo.db, ~/.claude/projects/) is
; deliberately preserved so reinstalling does not destroy the index.
Type: filesandordirs; Name: "{commonappdata}\mnemo"

