// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

final class AppDelegate: NSObject, NSApplicationDelegate {
    private var statusController: StatusItemController?

    func applicationDidFinishLaunching(_ notification: Notification) {
        let controller = StatusItemController()
        statusController = controller

        // Self-test driver (build-host verification via screencapture). Set
        // MNEMO_SHIM_SELFTEST=1 to open + hover, =click to also activate, or
        // =dashboard to open + snapshot the native status panel (🎯T86).
        if let mode = ProcessInfo.processInfo.environment["MNEMO_SHIM_SELFTEST"] {
            let t = Timer(timeInterval: 1.2, repeats: false) { _ in
                if mode == "dashboard" {
                    controller.selfTestDashboard()
                } else {
                    controller.selfTest(click: mode == "click")
                }
            }
            RunLoop.main.add(t, forMode: .common)
        }
    }
}
