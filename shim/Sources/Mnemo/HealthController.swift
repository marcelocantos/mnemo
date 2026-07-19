// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

// HealthController is the shim's consumer of the daemon's diagnostics (🎯T86).
// It primes from GET /health, then subscribes to GET /api/events and fans each
// event out to the three native surfaces:
//   • `health` snapshots → the status-item glyph (onSeverity) + the dashboard
//     window (live update);
//   • `alert` transitions → a native notification.
//
// Snapshots merge by check name: the daemon streams the Fast tier every few
// minutes and the Full tier hourly, so a fast snapshot updates only its checks
// while the rest keep their last-known state. Priming from /health (a full run)
// seeds every check immediately.
final class HealthController {
    private var stream: EventStream?
    private let notifications = NotificationManager()

    private var results: [String: HealthResult] = [:]
    private var generatedAt: String?

    // onSeverity reports the worst severity across all known checks, for the
    // status-item glyph. Called on the main thread.
    var onSeverity: ((Severity) -> Void)?
    // onReportUpdate pushes each merged snapshot to a live view (the Status tab),
    // when one is open. Called on the main thread.
    var onReportUpdate: ((HealthReport) -> Void)?
    // onOpenRequested fires when a health notification's action asks to show the
    // dashboard; the owner opens the window (StatusItemController).
    var onOpenRequested: (() -> Void)?
    // onPluginReload fires on the plugin.reload SSE event (🎯T102.9) so the
    // popover can re-request the plugin's live WKWebView document.
    var onPluginReload: ((String?) -> Void)?

    func start() {
        notifications.setOpenDashboard { [weak self] in self?.onOpenRequested?() }
        primeFromHealth()
        guard let url = DaemonClient.shared.eventsURL() else {
            Log.debug("HealthController: no events URL")
            return
        }
        let s = EventStream(url: url) { [weak self] name, data in
            MainThread.soon { self?.handle(name, data) }
        }
        stream = s
        s.start()
    }

    // refresh re-pulls a full /health report (the Status tab's Refresh button).
    func refresh() { primeFromHealth() }

    // ensureNotificationsAuthorized requests notification permission at a
    // contextual moment (the user opening the dashboard) rather than at launch.
    func ensureNotificationsAuthorized() { notifications.ensureAuthorized() }

    // MARK: - data

    private func primeFromHealth() {
        DaemonClient.shared.health { [weak self] report in
            guard let report = report else { return }
            MainThread.soon { self?.apply(report) }
        }
    }

    private func handle(_ name: String, _ data: Data) {
        switch name {
        case "health":
            if let r = try? JSONDecoder().decode(HealthReport.self, from: data) { apply(r) }
        case "alert":
            if let a = try? JSONDecoder().decode(HealthAlert.self, from: data) { notifications.post(a) }
        case "plugin.reload":
            // Empty body or missing name → reload whatever is showing.
            let pname = (try? JSONDecoder().decode(PluginReload.self, from: data))?.name
            onPluginReload?(pname)
        default:
            Log.debug("HealthController: unhandled event \(name)")
        }
    }

    private func apply(_ report: HealthReport) {
        for r in report.results { results[r.name] = r }
        if let g = report.generatedAt { generatedAt = g }
        onSeverity?(worst())
        onReportUpdate?(currentReport())
    }

    private func worst() -> Severity {
        results.values.map(\.sev).max(by: { $0.rawValue < $1.rawValue }) ?? .ok
    }

    // currentReport synthesises a HealthReport from the merged per-check state,
    // recomputing the counts so the dashboard summary matches what's shown.
    func currentReport() -> HealthReport {
        let all = Array(results.values)
        let ok = all.filter { $0.sev == .ok }.count
        let warn = all.filter { $0.sev == .warn }.count
        let fail = all.filter { $0.sev == .fail }.count
        return HealthReport(generatedAt: generatedAt, ok: ok, warn: warn, fail: fail, results: all)
    }
}
