// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import Foundation

// Health* mirror the daemon's diag.Report / diag.Result / diag.Alert JSON
// (🎯T86). They arrive two ways: a full report from GET /health (priming), and
// live `health` / `alert` events over GET /api/events.

// Severity is a check outcome, ordered ok < warn < fail — the raw values match
// so `max` finds the worst across a report.
enum Severity: Int {
    case ok = 0, warn = 1, fail = 2

    init(_ s: String) {
        switch s {
        case "fail": self = .fail
        case "warn": self = .warn
        default: self = .ok
        }
    }
}

struct HealthResult: Decodable {
    let name: String
    let severity: String // ok / warn / fail
    let tier: String // fast / full
    let detail: String?
    let remediation: String?

    var sev: Severity { Severity(severity) }
}

struct HealthReport: Decodable {
    let generatedAt: String?
    let ok: Int
    let warn: Int
    let fail: Int
    let results: [HealthResult]

    enum CodingKeys: String, CodingKey {
        case generatedAt = "generated_at"
        case ok, warn, fail, results
    }

    // worst is the most severe outcome across all checks (ok when empty) — the
    // signal the status-item glyph reflects.
    var worst: Severity { results.map(\.sev).max(by: { $0.rawValue < $1.rawValue }) ?? .ok }
}

// BudgetSnapshot is GET /api/budget (🎯T140) — spend + throttle without health list.
struct BudgetSnapshot: Decodable {
    let budget: BudgetFields?
    let throttle: ThrottleFields?

    struct BudgetFields: Decodable {
        let capUsd: Double?
        let spentUsd: Double?
        let spentPct: Double?
        let projectedPct: Double?
        let severity: String?
        let headline: String?
        let exhaustionDate: String?

        enum CodingKeys: String, CodingKey {
            case capUsd = "cap_usd"
            case spentUsd = "spent_usd"
            case spentPct = "spent_pct"
            case projectedPct = "projected_pct"
            case severity, headline
            case exhaustionDate = "exhaustion_date"
        }
    }

    struct ThrottleFields: Decodable {
        let level: String?
        let throttling: Bool?
        let detail: String?
        let remediation: String?
    }

    /// One-line menubar/tooltip summary.
    var menuSummary: String {
        let thr = throttle?.throttling == true
        let lvl = throttle?.level ?? "full"
        let spent = budget?.spentPct.map { String(format: "%.0f%% spent" , $0) } ?? "budget"
        if thr {
            return "THROTTLE \(lvl) · \(spent)"
        }
        return "\(spent) · throttle \(lvl)"
    }
}

// HealthAlert is a transition the daemon decided is worth a notification (its
// dedup + cooldown already applied). kind is "fail" or "recovery".
struct HealthAlert: Decodable {
    let name: String
    let severity: String
    let detail: String?
    let remediation: String?
    let kind: String
    let dashboardURL: String?

    enum CodingKeys: String, CodingKey {
        case name, severity, detail, remediation, kind
        case dashboardURL = "dashboard_url"
    }

    var isRecovery: Bool { kind == "recovery" }
}

// UIConfig is chrome for the multi-purpose shim: the process always runs;
// menu_bar_app only controls whether the status item is shown.
struct UIConfig: Decodable {
    let menuBarApp: Bool

    enum CodingKeys: String, CodingKey {
        case menuBarApp = "menu_bar_app"
    }
}
