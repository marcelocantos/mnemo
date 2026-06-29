// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import Foundation

// MainThread.soon schedules work on the main run loop via a zero-interval,
// non-repeating Timer. Under an accessory app's run loop, `Task { @MainActor }`
// and `DispatchQueue.main.async { }` dispatched from event handlers (or from
// URLSession callbacks) do not fire reliably; a Timer added to RunLoop.main
// does (§6 "Deferred main-thread work").
enum MainThread {
    static func soon(_ body: @escaping () -> Void) {
        let t = Timer(timeInterval: 0, repeats: false) { _ in body() }
        RunLoop.main.add(t, forMode: .common)
    }
}
