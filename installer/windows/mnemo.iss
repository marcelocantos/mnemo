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
OutputBaseFilename=mnemo-{#AppVersion}-windows-amd64-setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
PrivilegesRequired=admin
ArchitecturesInstallIn64BitMode=x64compatible
ArchitecturesAllowed=x64compatible
; Allow `uninstall-service` and `unregister-mcp` to find mnemo.exe via
; full path even if PATH is not updated.
UninstallDisplayIcon={app}\mnemo.exe
UninstallDisplayName=mnemo {#AppVersion}
LicenseFile={#SourceDir}\LICENSE.txt

[Files]
Source: "{#SourceDir}\mnemo.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceDir}\LICENSE.txt"; DestDir: "{app}"; Flags: ignoreversion

[Run]
; Service install (elevated — this is the installer's default
; context). Must run before register-mcp because register-mcp's
; `--url` default assumes the service is up on :19419 and Claude Code
; will connect shortly after.
Filename: "{app}\mnemo.exe"; Parameters: "install-service"; \
  StatusMsg: "Installing mnemo Windows Service..."; \
  Flags: runhidden waituntilterminated

; Start the service immediately so the user doesn't have to reboot.
; `net start` is used instead of sc.exe for universal availability;
; tolerate failure (service may already be running on upgrade).
Filename: "{sys}\net.exe"; Parameters: "start mnemo"; \
  StatusMsg: "Starting mnemo service..."; \
  Flags: runhidden waituntilterminated; \
  Check: ServiceShouldStart

; MCP registration (as the original user, so the right
; ~/.claude.json is patched). runasoriginaluser is critical: without
; it, the installer's elevated SYSTEM/Administrator context would
; patch a different user's profile — leaving the actual user with
; mnemo unregistered.
Filename: "{app}\mnemo.exe"; Parameters: "register-mcp"; \
  StatusMsg: "Registering mnemo with Claude Code..."; \
  Flags: runhidden waituntilterminated runasoriginaluser

[UninstallRun]
; Reverse order: unregister MCP first (as original user), then stop
; and remove the service. This order means Claude Code never sees a
; broken registration pointing at a stopped service.
Filename: "{app}\mnemo.exe"; Parameters: "unregister-mcp"; \
  Flags: runhidden waituntilterminated runasoriginaluser; \
  RunOnceId: "MnemoUnregisterMCP"

Filename: "{app}\mnemo.exe"; Parameters: "uninstall-service"; \
  Flags: runhidden waituntilterminated; \
  RunOnceId: "MnemoUninstallService"

[UninstallDelete]
; Drop the %ProgramData%\mnemo\ tree (logs, runtime state). The user's
; home-directory state (~/.mnemo/mnemo.db, ~/.claude/projects/) is
; deliberately preserved so reinstalling does not destroy the index.
Type: filesandordirs; Name: "{commonappdata}\mnemo"

[Code]
// ServiceShouldStart is a Check function for the `net start` line.
// We only try to start the service if the install-service step
// succeeded (the Filename: above runs before this; if it failed,
// Inno Setup would have already aborted the installer).
function ServiceShouldStart(): Boolean;
begin
  Result := True;
end;
