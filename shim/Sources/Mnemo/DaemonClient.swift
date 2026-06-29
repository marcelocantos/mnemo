// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import Foundation

// DaemonClient talks to the mnemo daemon's /api/thread/* endpoints over
// localhost HTTP (Integration §0.1, §0.4). All thread logic lives in the
// daemon; this is a transport shim. Completion handlers fire on a background
// queue — callers hop to the main thread via MainThread.soon before touching
// AppKit (the run-loop quirk in §6).
final class DaemonClient {
    static let shared = DaemonClient()

    private let session = URLSession(configuration: .ephemeral)

    // baseURL honours $MNEMO_ADDR (matching the Go CLI), defaulting to the
    // daemon's :19419.
    private let baseURL: String = {
        var addr = ProcessInfo.processInfo.environment["MNEMO_ADDR"] ?? ":19419"
        if addr.hasPrefix("http://") || addr.hasPrefix("https://") {
            return String(addr.reversed().drop { $0 == "/" }.reversed())
        }
        if addr.hasPrefix(":") { addr = "127.0.0.1" + addr }
        return "http://" + addr
    }()

    // list fetches the activity-sorted thread list plus the threads root.
    func list(_ done: @escaping (Result<ThreadList, Error>) -> Void) {
        get("/api/thread/list") { result in
            done(result.flatMap { data in
                Result { try JSONDecoder().decode(ThreadList.self, from: data) }
            })
        }
    }

    // setThreadsRoot points the threads folder at path (the settings gear).
    func setThreadsRoot(_ path: String, _ done: @escaping (Result<Void, Error>) -> Void) {
        post("/api/thread/config?root=\(escape(path))") { done($0.map { _ in () }) }
    }

    // threadsRoot fetches the current threads folder so the Settings tab is
    // self-contained regardless of which entry point opened the window.
    func threadsRoot(_ done: @escaping (String) -> Void) {
        get("/api/thread/config") { result in
            switch result {
            case .success(let data):
                let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: String]
                done(obj?["threads_root"] ?? "")
            case .failure:
                done("")
            }
        }
    }

    // preview fetches the standalone, theme-aware HTML for a thread.
    func preview(name: String, dark: Bool, _ done: @escaping (Result<String, Error>) -> Void) {
        let theme = dark ? "dark" : "light"
        get("/api/thread/preview?name=\(escape(name))&theme=\(theme)") { result in
            done(result.map { String(decoding: $0, as: UTF8.self) })
        }
    }

    // setMarker sets a thread's marker to a catalog value ("" clears it).
    func setMarker(name: String, marker: String, _ done: @escaping (Result<Void, Error>) -> Void) {
        post("/api/thread/set_marker?name=\(escape(name))&marker=\(escape(marker))") { done($0.map { _ in () }) }
    }

    // markers fetches the daemon's marker catalog so the UI builds its marker
    // menu without hardcoding the vocabulary.
    func markers(_ done: @escaping (Result<[MarkerInfo], Error>) -> Void) {
        get("/api/thread/markers") { result in
            done(result.flatMap { data in
                Result { try JSONDecoder().decode(MarkerCatalog.self, from: data).markers }
            })
        }
    }

    // go focuses or spawns the thread's iTerm2 tab via the daemon.
    func go(name: String, noResume: Bool, _ done: @escaping (Result<Void, Error>) -> Void) {
        var path = "/api/thread/go?name=\(escape(name))"
        if noResume { path += "&no_resume=1" }
        post(path) { done($0.map { _ in () }) }
    }

    // create scaffolds a new thread.
    func create(name: String, _ done: @escaping (Result<Void, Error>) -> Void) {
        post("/api/thread/new?name=\(escape(name))") { done($0.map { _ in () }) }
    }

    // search returns the names of threads whose content matches the query (the
    // deep channel). Failures yield an empty set so the instant channel stands.
    func search(query: String, _ done: @escaping ([String]) -> Void) {
        get("/api/thread/search?q=\(escape(query))") { result in
            switch result {
            case .success(let data):
                let names = (try? JSONDecoder().decode(SearchResult.self, from: data))?.names ?? []
                done(names)
            case .failure:
                done([])
            }
        }
    }

    // MARK: - health / events (🎯T86)

    // eventsURL is the SSE endpoint EventStream subscribes to.
    func eventsURL() -> URL? { URL(string: baseURL + "/api/events") }

    // dashboardURL is the web dashboard root, opened by "Open Full Dashboard…".
    func dashboardURL() -> URL? { URL(string: baseURL + "/") }

    // health pulls a full diagnostics report (GET /health) to prime the
    // dashboard + glyph before the first streamed snapshot, and on Refresh.
    func health(_ done: @escaping (HealthReport?) -> Void) {
        get("/health") { result in
            switch result {
            case .success(let data):
                done(try? JSONDecoder().decode(HealthReport.self, from: data))
            case .failure:
                done(nil)
            }
        }
    }

    // MARK: - transport

    private func get(_ path: String, _ done: @escaping (Result<Data, Error>) -> Void) {
        request(path, method: "GET", done)
    }

    private func post(_ path: String, _ done: @escaping (Result<Data, Error>) -> Void) {
        request(path, method: "POST", done)
    }

    private func request(_ path: String, method: String, _ done: @escaping (Result<Data, Error>) -> Void) {
        guard let url = URL(string: baseURL + path) else {
            done(.failure(ClientError.badURL)); return
        }
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.timeoutInterval = 15
        session.dataTask(with: req) { data, resp, err in
            if let err = err { done(.failure(err)); return }
            guard let http = resp as? HTTPURLResponse else {
                done(.failure(ClientError.noResponse)); return
            }
            let body = data ?? Data()
            guard (200..<300).contains(http.statusCode) else {
                let msg = String(decoding: body, as: UTF8.self)
                done(.failure(ClientError.http(http.statusCode, msg)))
                return
            }
            done(.success(body))
        }.resume()
    }

    private func escape(_ s: String) -> String {
        s.addingPercentEncoding(withAllowedCharacters: .urlQueryValueAllowed) ?? s
    }
}

enum ClientError: Error, CustomStringConvertible {
    case badURL
    case noResponse
    case http(Int, String)

    var description: String {
        switch self {
        case .badURL: return "bad URL"
        case .noResponse: return "no response from daemon"
        case .http(let code, let msg):
            return "daemon error \(code): \(msg.trimmingCharacters(in: .whitespacesAndNewlines))"
        }
    }
}

private extension CharacterSet {
    // urlQueryValueAllowed excludes & and = so query values encode safely.
    static let urlQueryValueAllowed: CharacterSet = {
        var cs = CharacterSet.urlQueryAllowed
        cs.remove(charactersIn: "&=?")
        return cs
    }()
}
