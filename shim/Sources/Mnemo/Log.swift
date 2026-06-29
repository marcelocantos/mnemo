// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import Foundation

// Log.debug writes to stderr when MNEMO_SHIM_DEBUG is set in the environment.
// Used to diagnose event-routing in the popover, which can't be inspected
// visually from the build host.
enum Log {
    static let enabled = ProcessInfo.processInfo.environment["MNEMO_SHIM_DEBUG"] != nil

    static func debug(_ message: @autoclosure () -> String) {
        guard enabled else { return }
        FileHandle.standardError.write(Data(("[shim] " + message() + "\n").utf8))
    }
}
