// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

// DashboardView renders the daemon's health report (🎯T86) — the same data
// behind the web dashboard's #health page and the mnemo_doctor tool. It is a
// plain NSView so it can live inside the Settings window's "Status" tab, and is
// live-updated from `health` events over the SSE stream.
//
// The report body is an attributed string in a read-only text view: free
// wrapping, trivial to rebuild on each update, and selectable so the user can
// copy a remediation hint. The footer holds the live actions.
final class DashboardView: NSView {
    private let textView = NSTextView()

    // onRefresh re-pulls a full /health report; onOpenFull opens the web view.
    var onRefresh: (() -> Void)?
    var onOpenFull: (() -> Void)?

    override init(frame frameRect: NSRect) {
        super.init(frame: frameRect)
        build()
    }

    required init?(coder: NSCoder) { fatalError("init(coder:) is unused") }

    func update(_ report: HealthReport) { render(report) }

    private func build() {
        let scroll = NSScrollView()
        scroll.translatesAutoresizingMaskIntoConstraints = false
        scroll.hasVerticalScroller = true
        scroll.drawsBackground = false
        scroll.borderType = .noBorder

        textView.isEditable = false
        textView.isSelectable = true
        textView.drawsBackground = false
        textView.textContainerInset = NSSize(width: 16, height: 14)
        textView.autoresizingMask = [.width]
        scroll.documentView = textView
        addSubview(scroll)

        let openFull = NSButton(title: "Open Full Dashboard…", target: self, action: #selector(openFull))
        openFull.bezelStyle = .rounded
        let refresh = NSButton(title: "Refresh", target: self, action: #selector(refresh))
        refresh.bezelStyle = .rounded

        let footer = NSStackView(views: [openFull, NSView(), refresh])
        footer.orientation = .horizontal
        footer.spacing = 8
        footer.translatesAutoresizingMaskIntoConstraints = false
        addSubview(footer)

        NSLayoutConstraint.activate([
            scroll.topAnchor.constraint(equalTo: topAnchor),
            scroll.leadingAnchor.constraint(equalTo: leadingAnchor),
            scroll.trailingAnchor.constraint(equalTo: trailingAnchor),
            scroll.bottomAnchor.constraint(equalTo: footer.topAnchor, constant: -10),

            footer.leadingAnchor.constraint(equalTo: leadingAnchor, constant: 16),
            footer.trailingAnchor.constraint(equalTo: trailingAnchor, constant: -16),
            footer.bottomAnchor.constraint(equalTo: bottomAnchor, constant: -14),
        ])
    }

    // MARK: - rendering

    private func render(_ report: HealthReport) {
        let s = NSMutableAttributedString()
        s.append(summaryLine(report))
        s.append(NSAttributedString(string: "\n" + updatedLine(report.generatedAt) + "\n\n",
                                    attributes: [.font: NSFont.systemFont(ofSize: 10),
                                                 .foregroundColor: NSColor.secondaryLabelColor]))
        for r in ordered(report.results) {
            s.append(checkBlock(r))
        }
        textView.textStorage?.setAttributedString(s)
    }

    // ordered sorts worst-first, then by name — failures and warnings rise to
    // the top where attention is due.
    private func ordered(_ results: [HealthResult]) -> [HealthResult] {
        results.sorted {
            $0.sev.rawValue != $1.sev.rawValue ? $0.sev.rawValue > $1.sev.rawValue : $0.name < $1.name
        }
    }

    private func summaryLine(_ r: HealthReport) -> NSAttributedString {
        let glyph: String
        let color: NSColor
        let text: String
        switch r.worst {
        case .fail:
            glyph = "✗"; color = .systemRed
            text = "\(r.fail) failing" + (r.warn > 0 ? ", \(r.warn) warning\(r.warn == 1 ? "" : "s")" : "")
        case .warn:
            glyph = "⚠"; color = .systemOrange
            text = "\(r.warn) warning\(r.warn == 1 ? "" : "s")"
        case .ok:
            glyph = "✓"; color = .systemGreen
            text = "All \(r.ok) checks healthy"
        }
        return NSAttributedString(string: "\(glyph)  \(text)",
                                  attributes: [.font: NSFont.boldSystemFont(ofSize: 15),
                                               .foregroundColor: color])
    }

    private func updatedLine(_ generatedAt: String?) -> String {
        guard let raw = generatedAt, let t = parseRFC3339(raw) else { return "live" }
        let f = DateFormatter()
        f.dateStyle = .none
        f.timeStyle = .medium
        return "updated \(f.string(from: t))"
    }

    private func checkBlock(_ r: HealthResult) -> NSAttributedString {
        let color = sevColor(r.sev)
        let block = NSMutableAttributedString()
        let head = NSMutableAttributedString(
            string: "● ",
            attributes: [.foregroundColor: color, .font: NSFont.systemFont(ofSize: 12)])
        head.append(NSAttributedString(
            string: r.name,
            attributes: [.font: NSFont.boldSystemFont(ofSize: 12), .foregroundColor: NSColor.labelColor]))
        head.append(NSAttributedString(
            string: "  (\(r.tier))\n",
            attributes: [.font: NSFont.systemFont(ofSize: 9), .foregroundColor: NSColor.tertiaryLabelColor]))
        block.append(head)

        if let detail = r.detail, !detail.isEmpty {
            block.append(NSAttributedString(
                string: detail + "\n",
                attributes: [.font: NSFont.systemFont(ofSize: 11), .foregroundColor: NSColor.secondaryLabelColor]))
        }
        if let rem = r.remediation, !rem.isEmpty {
            let italic = NSFontManager.shared.convert(NSFont.systemFont(ofSize: 11), toHaveTrait: .italicFontMask)
            block.append(NSAttributedString(
                string: "Fix: " + rem + "\n",
                attributes: [.font: italic, .foregroundColor: NSColor.systemOrange]))
        }
        block.append(NSAttributedString(string: "\n", attributes: [.font: NSFont.systemFont(ofSize: 4)]))
        return block
    }

    private func sevColor(_ s: Severity) -> NSColor {
        switch s {
        case .fail: return .systemRed
        case .warn: return .systemOrange
        case .ok: return .systemGreen
        }
    }

    private func parseRFC3339(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }

    // MARK: - actions

    @objc private func openFull() { onOpenFull?() }
    @objc private func refresh() { onRefresh?() }
}
