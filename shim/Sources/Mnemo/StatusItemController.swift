// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit
import ApplicationServices

// hotkeyDefaultsKey persists whether the user enabled the ⌥⌥ global hotkey.
private let hotkeyDefaultsKey = "hotkeyEnabled"

// StatusItemController owns the menu-bar status item, the popover, and the
// optional global hotkey (§6). The status item uses a FIXED square length so a
// variable-width glyph can't shift the popover anchor.
final class StatusItemController: NSObject, NSPopoverDelegate {
    private let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
    private let popover = NSPopover()
    private let content = PopoverContentController()
    // health drives the dashboard panel, native notifications, and the glyph's
    // colour from the daemon's diagnostics stream (🎯T86).
    private let health = HealthController()
    private var hotkey: HotkeyMonitor?
    // Dismissal monitors give the popover a menu-like lifecycle: any click
    // outside the app, or the app resigning active, closes it. We activate the
    // app + make the popover key (for Cmd-C), which suppresses .transient's own
    // click-outside dismissal, so we drive it explicitly here.
    private var globalClickMonitor: Any?
    private var resignObserver: Any?
    private var trustPoll: Timer?

    override init() {
        super.init()
        content.onRequestClose = { [weak self] in self?.closePopover() }

        if let button = statusItem.button {
            button.image = NSImage(systemSymbolName: "list.bullet", accessibilityDescription: "Threads")
            button.image?.isTemplate = true
            button.target = self
            button.action = #selector(statusClicked(_:))
            button.sendAction(on: [.leftMouseUp, .rightMouseUp])
        }

        // NSPopover routes scroll/click/focus natively and gives click-outside
        // dismissal for free with .transient — the single most important UI
        // substrate choice (§6). NSMenu and .nonactivatingPanel both break
        // scroll routing into the embedded preview.
        popover.behavior = .transient
        popover.animates = false
        popover.contentViewController = content
        popover.delegate = self

        // The popover footer opens the settings window on the Settings / Status
        // tabs (🎯T86).
        content.onOpenDashboard = { [weak self] in self?.openDashboard() }
        content.onOpenSettings = { [weak self] in self?.openSettings() }

        // Prewarm the list/previews on launch so the first open is instant.
        content.prewarm()

        // Subscribe to the daemon's diagnostics stream: tint the glyph by worst
        // severity, feed the Status tab, and raise native notifications (🎯T86).
        health.onSeverity = { [weak self] sev in self?.applySeverity(sev) }
        // A notification's "Open Dashboard" action opens the Status tab.
        health.onOpenRequested = { [weak self] in self?.showSettings(select: .status) }
        health.start()

        // Restore the opt-in hotkey across launches (the daemon relaunches the
        // shim, so persistence is what keeps it "on"). No permission prompt on
        // restore — only when the user actively enables it.
        if UserDefaults.standard.bool(forKey: hotkeyDefaultsKey) {
            startHotkey(requestPermission: false)
        }
    }

    // applySeverity sets the status-item glyph by the worst current health
    // severity: a red exclamation on failure, an orange glyph on warnings, the
    // plain glyph when healthy. (🎯T86)
    //
    // It must NOT touch button.contentTintColor. Setting contentTintColor on an
    // NSStatusBarButton — even back to nil — removes the button from the
    // system's automatic menu-bar template adaptation, so the template renders
    // in the base (light) appearance: a black glyph that is invisible on a dark
    // menu bar. (That was the T86 regression.) Instead:
    //   - healthy: a pure template image (isTemplate = true), no tint, so the
    //     system colours it for the current menu-bar appearance (white on dark);
    //   - warn/fail: the colour baked into the symbol via a palette
    //     configuration with isTemplate = false, so the alert colour shows on
    //     any menu-bar appearance without going through contentTintColor.
    // The glyph keeps a fixed square length so the popover anchor never shifts.
    private func applySeverity(_ sev: Severity) {
        guard let button = statusItem.button else { return }
        switch sev {
        case .ok:
            button.image = Self.templateGlyph("list.bullet", "Threads")
        case .warn:
            button.image = Self.colouredGlyph("list.bullet", "mnemo: health warnings", .systemOrange)
        case .fail:
            button.image = Self.colouredGlyph("exclamationmark.triangle.fill", "mnemo: health failing", .systemRed)
        }
        Log.debug("glyph severity=\(sev)")
    }

    // templateGlyph builds an adaptive (template) menu-bar glyph that the system
    // recolours for the current menu-bar appearance.
    private static func templateGlyph(_ symbol: String, _ label: String) -> NSImage? {
        let img = NSImage(systemSymbolName: symbol, accessibilityDescription: label)
        img?.isTemplate = true
        return img
    }

    // colouredGlyph bakes a fixed colour into the symbol (non-template) so an
    // alert state is visible on any menu-bar appearance without contentTintColor.
    private static func colouredGlyph(_ symbol: String, _ label: String, _ colour: NSColor) -> NSImage? {
        let cfg = NSImage.SymbolConfiguration(paletteColors: [colour])
        let img = NSImage(systemSymbolName: symbol, accessibilityDescription: label)?
            .withSymbolConfiguration(cfg)
        img?.isTemplate = false
        return img
    }

    // openDashboard dismisses the popover (if open) and shows the settings
    // window on the Status tab — used by the popover footer and the right-click
    // menu. Notification actions reach the same tab via health.onOpenRequested.
    private func openDashboard() {
        closePopover()
        health.ensureNotificationsAuthorized()
        showSettings(select: .status)
    }

    // openSettings dismisses the popover and shows the Settings tab (the gear).
    private func openSettings() {
        closePopover()
        showSettings(select: .settings)
    }

    // showSettings opens (or focuses) the shared settings window on a tab,
    // wiring the threads-folder change to refresh the popover list.
    private func showSettings(select: SettingsWindowController.Tab) {
        SettingsWindowController.show(health: health, onChange: { [weak self] in
            MainThread.soon { self?.content.reload() }
        }, select: select)
    }

    @objc private func statusClicked(_ sender: NSStatusBarButton) {
        let event = NSApp.currentEvent
        if event?.type == .rightMouseUp {
            showMenu()
            return
        }
        toggle()
    }

    func toggle() {
        if popover.isShown {
            closePopover()
        } else if let button = statusItem.button {
            NSApp.activate(ignoringOtherApps: true)
            popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
            // Make the popover window key so Cmd-C/V work in the non-key
            // transient popover (§6 "Copy / paste in a non-key popover").
            popover.contentViewController?.view.window?.makeKey()
            content.popoverDidOpen()
            installDismissMonitors()
        }
    }

    private func closePopover() {
        if popover.isShown { popover.performClose(nil) }
    }

    // installDismissMonitors makes the popover behave like a menu: a mouse-down
    // anywhere outside the app (global monitor — mouse events need no
    // Accessibility grant), or the app resigning active, closes it. Clicks
    // inside the popover are local events, so they don't trip the monitor.
    private func installDismissMonitors() {
        removeDismissMonitors()
        globalClickMonitor = NSEvent.addGlobalMonitorForEvents(
            matching: [.leftMouseDown, .rightMouseDown, .otherMouseDown]
        ) { [weak self] _ in
            self?.closePopover()
        }
        resignObserver = NotificationCenter.default.addObserver(
            forName: NSApplication.didResignActiveNotification, object: nil, queue: .main
        ) { [weak self] _ in
            self?.closePopover()
        }
    }

    private func removeDismissMonitors() {
        if let m = globalClickMonitor { NSEvent.removeMonitor(m) }
        globalClickMonitor = nil
        if let o = resignObserver { NotificationCenter.default.removeObserver(o) }
        resignObserver = nil
    }

    // popoverDidClose fires for every close path (status-item toggle, row
    // activation, outside click, resign-active), so monitor cleanup lives here.
    func popoverDidClose(_ notification: Notification) {
        removeDismissMonitors()
        content.popoverWillClose()
    }

    // MARK: - right-click menu (hotkey opt-in + quit)

    private func showMenu() {
        let menu = NSMenu()
        let dashboard = NSMenuItem(title: "Dashboard…", action: #selector(showDashboard), keyEquivalent: "")
        dashboard.target = self
        menu.addItem(dashboard)
        menu.addItem(.separator())
        let toggleItem = NSMenuItem(
            title: hotkey == nil ? "Enable global hotkey (⌥⌥)" : "Disable global hotkey",
            action: #selector(toggleHotkey), keyEquivalent: "")
        toggleItem.target = self
        menu.addItem(toggleItem)
        menu.addItem(.separator())
        let quit = NSMenuItem(title: "Quit", action: #selector(quit), keyEquivalent: "q")
        quit.target = self
        menu.addItem(quit)

        statusItem.menu = menu
        statusItem.button?.performClick(nil)
        statusItem.menu = nil // one-shot; restore click-to-toggle behaviour
    }

    // toggleHotkey starts/stops the opt-in ⌥-double-tap monitor and persists
    // the choice. Enabling it is what triggers the Accessibility request — a
    // default install never asks (§0.9).
    @objc private func toggleHotkey() {
        if hotkey == nil {
            startHotkey(requestPermission: true)
            UserDefaults.standard.set(true, forKey: hotkeyDefaultsKey)
        } else {
            hotkey?.stop()
            hotkey = nil
            UserDefaults.standard.set(false, forKey: hotkeyDefaultsKey)
        }
    }

    // startHotkey installs the global ⌥⌥ monitor. A global keyboard monitor
    // only delivers events when the app is trusted for Accessibility, so when
    // the user enables it we prompt for that permission. A monitor added while
    // untrusted won't begin firing on its own once the grant lands, so we poll
    // for trust and re-install it.
    private func startHotkey(requestPermission: Bool) {
        let trusted = AXIsProcessTrusted()
        if requestPermission && !trusted {
            let opts = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
            _ = AXIsProcessTrustedWithOptions(opts)
            let alert = NSAlert()
            alert.messageText = "Enable the ⌥⌥ shortcut"
            alert.informativeText = "Turn on “Mnemo” under System Settings → Privacy & Security → Accessibility. Then double-tapping the Option key anywhere will open this menu."
            alert.addButton(withTitle: "OK")
            NSApp.activate(ignoringOtherApps: true)
            alert.runModal()
        }
        installHotkeyMonitor()
        if !trusted { pollForTrust() }
    }

    private func installHotkeyMonitor() {
        hotkey?.stop()
        hotkey = HotkeyMonitor { [weak self] in self?.toggle() }
        hotkey?.start()
    }

    // pollForTrust re-installs the monitor once the user grants Accessibility
    // (up to ~2 min), so the shortcut works without relaunching.
    private func pollForTrust() {
        trustPoll?.invalidate()
        var ticks = 0
        let timer = Timer(timeInterval: 1.0, repeats: true) { [weak self] t in
            ticks += 1
            guard let self = self, self.hotkey != nil else { t.invalidate(); return }
            if AXIsProcessTrusted() {
                t.invalidate()
                self.installHotkeyMonitor()
            } else if ticks > 120 {
                t.invalidate()
            }
        }
        RunLoop.main.add(timer, forMode: .common)
        trustPoll = timer
    }

    @objc private func showDashboard() { openDashboard() }

    @objc private func quit() {
        NSApp.terminate(nil)
    }

    // selfTest (MNEMO_SHIM_SELFTEST) drives the popover from code so the build
    // host can verify the UI via screencapture without a human at the mouse.
    // It opens the popover, hovers the first row (loading its preview), and —
    // when MNEMO_SHIM_SELFTEST=click — activates it.
    // selfTestDashboard opens the Status tab and snapshots it, for screencapture
    // verification of the T86 status panel.
    func selfTestDashboard() {
        showSettings(select: .status)
        after(1.5) {
            SettingsWindowController.current?.snapshot(to: "/tmp/dashboard.png")
        }
    }

    func selfTest(click: Bool) {
        toggle()
        after(1.0) { [weak self] in
            self?.content.selfTestHoverFirst()
            after(0.8) { [weak self] in
                self?.content.snapshot(to: "/tmp/popover.png")
                if click { self?.content.selfTestActivateFirst() }
            }
        }
    }
}

// after schedules body on the main run loop after delay seconds, using a Timer
// (reliable under the accessory app's run loop where dispatch can be flaky).
func after(_ delay: TimeInterval, _ body: @escaping () -> Void) {
    let t = Timer(timeInterval: delay, repeats: false) { _ in body() }
    RunLoop.main.add(t, forMode: .common)
}
