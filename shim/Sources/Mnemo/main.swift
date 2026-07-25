// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

// Entry point for the multi-purpose Mnemo native shim (🎯T85.4). Always
// presents health notifications; optional menu-bar chrome is toggled by
// the daemon's retained "ui" event. Runs as an accessory app (no Dock
// icon, LSUIElement-equivalent) via setActivationPolicy(.accessory).
let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let delegate = AppDelegate()
app.delegate = delegate
app.run()
