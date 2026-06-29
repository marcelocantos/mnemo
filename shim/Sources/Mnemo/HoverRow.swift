// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

// HoverRow is a full-width, clickable footer row that highlights on hover the
// same way thread rows do, so the footer actions read as part of the same list
// rather than a cramped button strip (§6).
final class HoverRow: NSView {
    private let action: () -> Void
    private let onHover: (Bool) -> Void
    private var tracking: NSTrackingArea?

    init(title: String, action: @escaping () -> Void, onHover: @escaping (Bool) -> Void) {
        self.action = action
        self.onHover = onHover
        super.init(frame: .zero)
        wantsLayer = true

        let label = NSTextField(labelWithString: title)
        label.textColor = .labelColor
        label.translatesAutoresizingMaskIntoConstraints = false
        addSubview(label)
        NSLayoutConstraint.activate([
            label.leadingAnchor.constraint(equalTo: leadingAnchor, constant: 10),
            label.centerYAnchor.constraint(equalTo: centerYAnchor),
        ])
    }

    required init?(coder: NSCoder) { fatalError("not used") }

    // hitTest claims the whole row so clicks over the label still reach us.
    override func hitTest(_ point: NSPoint) -> NSView? {
        guard let parent = superview else { return nil }
        return bounds.contains(convert(point, from: parent)) ? self : nil
    }

    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        if let t = tracking { removeTrackingArea(t) }
        let t = NSTrackingArea(
            rect: .zero, options: [.mouseEnteredAndExited, .activeAlways, .inVisibleRect], owner: self)
        addTrackingArea(t)
        tracking = t
    }

    override func mouseEntered(with event: NSEvent) {
        layer?.backgroundColor = HoverRow.highlight
        onHover(true)
    }

    override func mouseExited(with event: NSEvent) {
        layer?.backgroundColor = NSColor.clear.cgColor
        onHover(false)
    }

    override func mouseUp(with event: NSEvent) {
        if bounds.contains(convert(event.locationInWindow, from: nil)) { action() }
    }

    // highlight matches the thread-row hover tint.
    static let highlight = NSColor.selectedContentBackgroundColor.withAlphaComponent(0.25).cgColor
}
