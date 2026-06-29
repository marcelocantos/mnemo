# Packaging the Threads menu-bar shim (🎯T85.5)

The Threads feature has two macOS components, both shipped by the one mnemo
install:

- the **mnemo daemon** (Go binary) — already installed via the Homebrew formula
  and started by `brew services` as a LaunchAgent (GUI session);
- the **Mnemo.app** menu-bar shim (Swift/AppKit) — a signed `.app` the
  daemon launches via `open -g` and keeps alive (Integration §0.1).

There is **no second LaunchAgent and no separate GUI install step**: starting
the daemon brings up the menu-bar app automatically.

## Build the app

```bash
shim/build-app.sh                 # → shim/dist/Mnemo.app (ad-hoc signed)
```

For distribution, sign with a Developer ID and notarize so Gatekeeper does not
block first launch:

```bash
MNEMO_SIGN_ID="Developer ID Application: <name> (<team>)" shim/build-app.sh
ditto -c -k --keepParent shim/dist/Mnemo.app shim/dist/Mnemo.app.zip
xcrun notarytool submit shim/dist/Mnemo.app.zip --keychain-profile <profile> --wait
xcrun stapler staple shim/dist/Mnemo.app
```

## Homebrew formula delta (marcelocantos/homebrew-tap)

The `mnemo` formula installs the daemon today. To ship the shim with it, add the
app under `libexec` (the daemon probes `<prefix>/opt/mnemo/libexec/Mnemo.app`,
see `resolveThreadsApp` in `threads_shim.go`):

```ruby
def install
  # ... existing daemon build/install ...
  system "./shim/build-app.sh", "#{buildpath}/shim/dist"   # or ship a prebuilt, notarized .app
  libexec.install "shim/dist/Mnemo.app"
end
```

No change to the `service` block is needed — the daemon launches and supervises
the app itself. The shim can also be pointed at an arbitrary build via the
`MNEMO_THREADS_APP` environment variable (overrides the probe).

## TCC (permissions)

Two **independent** code signatures, two grants — TCC is never inherited from a
parent process:

| Process       | Grant                       | When prompted                                  |
|---------------|-----------------------------|------------------------------------------------|
| mnemo daemon  | Automation → iTerm2         | first `thread go` (drives iTerm2 via osascript)|
| Mnemo.app| Accessibility (opt-in only) | only when the user enables the ⌥⌥ global hotkey|

A **default install grants nothing**: the menu-bar click needs no permission,
the global hotkey is opt-in, and the single Automation prompt appears only on
the first `go`. Driving iTerm2 via AppleScript (not its Python API) means there
is no "enable the Python API" step at all (§4, §0.9).
