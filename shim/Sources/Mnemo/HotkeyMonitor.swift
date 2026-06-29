// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

// HotkeyMonitor implements an opt-in global double-tap-Option (⌥⌥) toggle via a
// global NSEvent monitor (§6). It is a clean double-tap state machine: each tap
// held < maxHold, both taps within maxGap, and any other key or modifier
// resets it. Left/right Option are keycodes 58/61. The global monitor requires
// Accessibility permission — starting this is what raises the prompt (§0.9), so
// it is never started on a default install.
final class HotkeyMonitor {
    private let onTrigger: () -> Void
    private var monitor: Any?

    private let leftOption: UInt16 = 58
    private let rightOption: UInt16 = 61
    private let maxHold: TimeInterval = 0.3
    private let maxGap: TimeInterval = 0.3

    private var pressStart: Date?
    private var lastTapEnd: Date?

    init(onTrigger: @escaping () -> Void) {
        self.onTrigger = onTrigger
    }

    func start() {
        guard monitor == nil else { return }
        monitor = NSEvent.addGlobalMonitorForEvents(matching: .flagsChanged) { [weak self] event in
            self?.handle(event)
        }
    }

    func stop() {
        if let m = monitor { NSEvent.removeMonitor(m) }
        monitor = nil
        pressStart = nil
        lastTapEnd = nil
    }

    deinit { stop() }

    private func handle(_ event: NSEvent) {
        let isOption = event.keyCode == leftOption || event.keyCode == rightOption
        let optionDown = event.modifierFlags.contains(.option)
        let otherModifier = event.modifierFlags
            .intersection([.command, .control, .shift, .function, .capsLock])
        if !isOption || !otherModifier.isEmpty {
            reset()
            return
        }
        let now = Date()
        if optionDown {
            pressStart = now
            return
        }
        // Option released: complete a tap iff it was held briefly.
        guard let start = pressStart, now.timeIntervalSince(start) <= maxHold else {
            reset()
            return
        }
        pressStart = nil
        if let prev = lastTapEnd, now.timeIntervalSince(prev) <= maxGap {
            lastTapEnd = nil
            MainThread.soon(onTrigger)
        } else {
            lastTapEnd = now
        }
    }

    private func reset() {
        pressStart = nil
        lastTapEnd = nil
    }
}
