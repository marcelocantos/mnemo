// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

// ThreadTableView is the thread list. A table view (not a stack view in a
// scroll view) owns its own scroll layout, top-anchors naturally, reuses rows,
// and keeps scroll position stable across content changes (§6).
//
// Hover uses a SINGLE tracking area over the visible rect plus mouseMoved
// resolving row(at:), with one hoveredRow as the source of truth. Per-row
// mouseEntered/mouseExited fire faster than they pair during fast scrolls and
// leave multiple rows stuck highlighted (§6).
final class ThreadTableView: NSTableView {
    var onHoverRow: ((Int) -> Void)?
    // contextMenuForRow returns the right-click menu for the row under the
    // cursor (used to change a thread's marker).
    var contextMenuForRow: ((Int) -> NSMenu?)?

    private var trackingArea: NSTrackingArea?

    override func menu(for event: NSEvent) -> NSMenu? {
        let r = row(at: convert(event.locationInWindow, from: nil))
        guard r >= 0 else { return nil }
        return contextMenuForRow?(r)
    }

    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        if let existing = trackingArea { removeTrackingArea(existing) }
        let area = NSTrackingArea(
            rect: .zero,
            options: [.mouseMoved, .mouseEnteredAndExited, .activeAlways, .inVisibleRect],
            owner: self, userInfo: nil)
        addTrackingArea(area)
        trackingArea = area
        Log.debug("updateTrackingAreas visibleRect=\(visibleRect) rows=\(numberOfRows)")
    }

    override func mouseMoved(with event: NSEvent) {
        let point = convert(event.locationInWindow, from: nil)
        let r = row(at: point)
        Log.debug("mouseMoved point=\(point) row=\(r)")
        onHoverRow?(r)
    }

    override func mouseExited(with event: NSEvent) {
        // The preview pane sits directly to the LEFT of the list. Exiting that
        // way means the user is moving onto the preview to read or scroll it, so
        // keep it (and the row's highlight) up. Any other exit — below the rows,
        // down to the footer, up to the search field — clears, so the preview
        // only ever changes or disappears when the cursor moves to another row
        // or off the list entirely. Clearing *within* the list (empty space
        // below the rows) is handled by mouseMoved resolving row -1.
        let point = convert(event.locationInWindow, from: nil)
        if point.x < bounds.minX { return }
        onHoverRow?(-1)
    }
}
