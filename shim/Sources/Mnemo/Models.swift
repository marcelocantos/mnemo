// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import Foundation

// ThreadView mirrors the JSON the daemon returns from /api/thread/list (and
// the threads.View Go type). Optional fields are omitted by the server when
// empty.
struct ThreadView: Decodable {
    let name: String
    let path: String
    let state: String
    let status: String
    let focus: String?
    let fileCount: Int
    let activity: String?
    let activitySummary: String
    let compactAge: String?
    let marker: String?
    let markerEmoji: String?
    let activeTodos: Int?
    let overdueTodos: Int?

    enum CodingKeys: String, CodingKey {
        case name, path, state, status, focus
        case fileCount = "file_count"
        case activity
        case activitySummary = "activity_summary"
        case compactAge = "compact_age"
        case marker
        case markerEmoji = "marker_emoji"
        case activeTodos = "active_todos"
        case overdueTodos = "overdue_todos"
    }

    // Count strings for the columns: blank when zero (omitted by the daemon).
    var activeText: String { (activeTodos ?? 0) > 0 ? "\(activeTodos!)" : "" }
    var overdueText: String { (overdueTodos ?? 0) > 0 ? "\(overdueTodos!)" : "" }

    var glyph: String { markerEmoji ?? "🪡" }
    var age: String { compactAge ?? "" }
}

// MarkerInfo / MarkerCatalog mirror the daemon's /api/thread/markers response
// (internal/threads.MarkerInfo). The marker vocabulary lives in the daemon; the
// UI builds its marker menu from this catalog rather than hardcoding values.
struct MarkerInfo: Decodable {
    let value: String
    let emoji: String
    let label: String
    let pinned: Bool
}

struct MarkerCatalog: Decodable {
    let markers: [MarkerInfo]
}

// ThreadList is the /api/thread/list envelope.
struct ThreadList: Decodable {
    let root: String
    let count: Int
    let threads: [ThreadView]
}

// SearchResult is the /api/thread/search envelope (the deep channel).
struct SearchResult: Decodable {
    let names: [String]
}

// PluginUIContribution mirrors GET /api/plugins entries (🎯T102.9).
// Paths are same-origin (/plugins/<name>/…) for the WKWebView.
struct PluginUIContribution: Decodable {
    let name: String
    let label: String
    let icon: String?
    let previewURL: String?
    let pageURL: String?
    let menu: String?
    let version: String?
    let description: String?

    enum CodingKeys: String, CodingKey {
        case name, label, icon, menu, version, description
        case previewURL = "preview_url"
        case pageURL = "page_url"
    }
}

struct PluginList: Decodable {
    let count: Int
    let plugins: [PluginUIContribution]
}

// PluginReload is the data payload of the plugin.reload SSE event.
struct PluginReload: Decodable {
    let name: String?
}
