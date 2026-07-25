// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

final class AppDelegate: NSObject, NSApplicationDelegate {
    // Health + notifications always run — the shim is multi-purpose and is
    // the sole presenter for daemon health alerts. Menu-bar chrome is optional.
    private var health: HealthController?
    private var statusController: StatusItemController?

    func applicationDidFinishLaunching(_ notification: Notification) {
        let hc = HealthController()
        health = hc
        hc.onOpenRequested = { [weak self] in
            self?.openDashboard()
        }
        hc.onUIConfig = { [weak self] ui in
            self?.applyMenuBar(ui.menuBarApp)
        }
        hc.start()

        // Self-test driver (build-host verification via screencapture). Set
        // MNEMO_SHIM_SELFTEST=1 to open + hover, =click to also activate, or
        // =dashboard to open + snapshot the native status panel (🎯T86).
        if let mode = ProcessInfo.processInfo.environment["MNEMO_SHIM_SELFTEST"] {
            // Self-tests expect a status item; force menu bar on.
            applyMenuBar(true)
            let t = Timer(timeInterval: 1.2, repeats: false) { [weak self] _ in
                guard let sc = self?.statusController else { return }
                if mode == "dashboard" {
                    sc.selfTestDashboard()
                } else {
                    sc.selfTest(click: mode == "click")
                }
            }
            RunLoop.main.add(t, forMode: .common)
        }
    }

    // applyMenuBar shows or hides the status item without affecting
    // notifications or the health stream.
    private func applyMenuBar(_ on: Bool) {
        if on {
            if statusController == nil, let health = health {
                statusController = StatusItemController(health: health)
            }
        } else {
            statusController = nil
        }
        Log.debug("menu_bar_app=\(on)")
    }

    private func openDashboard() {
        guard let health = health else { return }
        health.ensureNotificationsAuthorized()
        SettingsWindowController.show(health: health, onChange: {
            MainThread.soon { /* threads list lives on status item when present */ }
        }, select: .status)
    }
}
