// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit
import UserNotifications

// NotificationManager posts native health notifications (🎯T86). The daemon
// already decides *when* to notify (threshold, dedup, cooldown) and streams the
// decision as an `alert` event; this turns that into a UNNotification with an
// "Open Dashboard" action.
//
// Authorization is requested lazily — on the first alert or when the user opens
// the dashboard — so a healthy default install never raises an unsolicited
// prompt (the project's minimal-friction ethos, §0.9). UNUserNotificationCenter
// needs a real bundle identity, which the signed Mnemo.app has but a bare SPM
// dev binary does not; in that case we fall back to `osascript` so development
// runs still surface alerts.
final class NotificationManager: NSObject, UNUserNotificationCenterDelegate {
    private let categoryID = "HEALTH"
    private let openActionID = "OPEN_DASHBOARD"
    private var requested = false
    private var onOpenDashboard: (() -> Void)?

    // canUseUN gates the modern API on a real app bundle; a dev binary has no
    // bundle identifier and would trap in UNUserNotificationCenter.current().
    private var canUseUN: Bool { Bundle.main.bundleIdentifier != nil }

    // setOpenDashboard wires the "Open Dashboard" action / notification click.
    func setOpenDashboard(_ handler: @escaping () -> Void) {
        onOpenDashboard = handler
    }

    // ensureAuthorized registers the category + delegate and requests
    // authorization exactly once. Safe to call from any contextual entry point.
    func ensureAuthorized() {
        guard canUseUN, !requested else { return }
        requested = true
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        let open = UNNotificationAction(identifier: openActionID, title: "Open Dashboard", options: [.foreground])
        let cat = UNNotificationCategory(identifier: categoryID, actions: [open], intentIdentifiers: [], options: [])
        center.setNotificationCategories([cat])
        center.requestAuthorization(options: [.alert, .sound]) { granted, err in
            if let err = err { Log.debug("notification auth error: \(err)") }
            Log.debug("notification authorization granted=\(granted)")
        }
    }

    // post surfaces an alert. Recovery alerts get a terse body; failures carry
    // the detail and remediation, matching the daemon's headless wording.
    func post(_ alert: HealthAlert) {
        let title = alert.isRecovery
            ? "mnemo: \(alert.name) recovered"
            : "mnemo: \(alert.name) \(alert.severity)"
        let body = alert.isRecovery ? "This check is healthy again." : failBody(alert)

        guard canUseUN else {
            osascriptFallback(title: title, body: body)
            return
        }
        ensureAuthorized()
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.categoryIdentifier = categoryID
        // A stable id per (check, kind) lets a repeat fail replace rather than
        // stack — the daemon's cooldown already rate-limits repeats.
        let id = "health.\(alert.name).\(alert.kind)"
        let req = UNNotificationRequest(identifier: id, content: content, trigger: nil)
        UNUserNotificationCenter.current().add(req) { err in
            if let err = err { Log.debug("notification add error: \(err)") }
        }
    }

    private func failBody(_ a: HealthAlert) -> String {
        var lines: [String] = []
        if let d = a.detail, !d.isEmpty { lines.append(d) }
        if let r = a.remediation, !r.isEmpty { lines.append("Fix: \(r)") }
        return lines.joined(separator: "\n")
    }

    // MARK: - UNUserNotificationCenterDelegate

    // Show the banner even though the accessory app is "active" — without this
    // a foreground app suppresses its own notifications.
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner, .sound])
    }

    // Clicking the notification (or its "Open Dashboard" action) opens the
    // native dashboard panel.
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        if response.actionIdentifier == openActionID || response.actionIdentifier == UNNotificationDefaultActionIdentifier {
            MainThread.soon { [weak self] in self?.onOpenDashboard?() }
        }
        completionHandler()
    }

    // MARK: - dev fallback

    private func osascriptFallback(title: String, body: String) {
        // osascript notification bodies are single line; collapse newlines.
        let oneLine = body.replacingOccurrences(of: "\n", with: " — ")
        let script = "display notification \(quote(oneLine)) with title \(quote(title))"
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
        p.arguments = ["-e", script]
        do { try p.run() } catch { Log.debug("osascript fallback failed: \(error)") }
    }

    // quote produces a safe double-quoted AppleScript string literal.
    private func quote(_ s: String) -> String {
        "\"" + s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\""
    }
}
