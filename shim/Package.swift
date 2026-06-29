// swift-tools-version:5.9
// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import PackageDescription

// Mnemo is mnemo's menu-bar app (🎯T85.4) — a thin AppKit view layer over the
// mnemo daemon's HTTP API, with all business logic in the daemon (Integration
// §0.1). Threads are its first and most prominent surface, but the app is
// mnemo's, not a threads-specific tool, and is meant to grow more features.
// Built as an SPM executable for development; T85.5 wraps it in a signed .app.
let package = Package(
    name: "Mnemo",
    platforms: [.macOS(.v13)],
    targets: [
        // tools-version 5.9 builds in Swift 5 language mode by default, which
        // keeps AppKit's main-thread idioms free of Swift 6 strict-concurrency
        // friction.
        .executableTarget(
            name: "Mnemo",
            path: "Sources/Mnemo"
        )
    ]
)
