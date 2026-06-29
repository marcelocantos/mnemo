// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

// Entry point for the Mnemo menu-bar shim (🎯T85.4). Runs as an accessory
// app (no Dock icon, LSUIElement-equivalent) via setActivationPolicy(.accessory)
// so a development build behaves like the bundled .app without an Info.plist.
let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let delegate = AppDelegate()
app.delegate = delegate
app.run()
