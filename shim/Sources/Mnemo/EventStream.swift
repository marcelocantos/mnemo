// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import Foundation

// EventStream is a Server-Sent-Events client for the daemon's GET /api/events
// (🎯T86). It holds one long-lived streaming connection, parses SSE frames
// (`event:` + `data:` lines terminated by a blank line), and hands each
// (event-name, data) pair to onEvent. It reconnects with exponential backoff
// when the connection drops — the daemon restarts, a laptop wakes, etc.
//
// The delegate callbacks arrive on URLSession's background queue; onEvent
// consumers hop to the main thread themselves (MainThread.soon), matching the
// rest of the shim. Reconnect scheduling hops to main because it touches
// RunLoop.main via `after`.
final class EventStream: NSObject, URLSessionDataDelegate {
    private let url: URL
    private let onEvent: (String, Data) -> Void

    private var session: URLSession!
    private var task: URLSessionDataTask?
    private var stopped = false
    private var backoff = 1.0

    // Frame-assembly state (mutated only on the session's serial delegate queue).
    private var lineBuffer = Data()
    private var eventName = ""
    private var dataBuffer = Data()

    private let maxBackoff = 30.0

    init(url: URL, onEvent: @escaping (String, Data) -> Void) {
        self.url = url
        self.onEvent = onEvent
        super.init()
        let cfg = URLSessionConfiguration.ephemeral
        // The server sends a keepalive comment every 25 s, so a per-request
        // timeout comfortably above that won't trip on an idle-but-live stream;
        // the resource timeout is disabled so the stream can run indefinitely.
        cfg.timeoutIntervalForRequest = 120
        cfg.timeoutIntervalForResource = .infinity
        session = URLSession(configuration: cfg, delegate: self, delegateQueue: nil)
    }

    func start() {
        stopped = false
        connect()
    }

    func stop() {
        stopped = true
        task?.cancel()
    }

    private func connect() {
        lineBuffer.removeAll(keepingCapacity: true)
        eventName = ""
        dataBuffer.removeAll(keepingCapacity: true)
        var req = URLRequest(url: url)
        req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        let t = session.dataTask(with: req)
        task = t
        t.resume()
    }

    // MARK: - URLSessionDataDelegate

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        backoff = 1.0 // a live stream resets backoff
        lineBuffer.append(data)
        // Process every complete LF-terminated line; a partial trailing line
        // stays buffered for the next chunk.
        while let nl = lineBuffer.firstIndex(of: 0x0A) {
            let lineData = lineBuffer.subdata(in: lineBuffer.startIndex ..< nl)
            lineBuffer.removeSubrange(lineBuffer.startIndex ... nl)
            var line = String(decoding: lineData, as: UTF8.self)
            if line.hasSuffix("\r") { line.removeLast() } // tolerate CRLF
            handle(line: line)
        }
    }

    private func handle(line: String) {
        if line.isEmpty {
            dispatchFrame()
        } else if line.hasPrefix(":") {
            // Comment / keepalive — ignore.
        } else if let v = field("event:", line) {
            eventName = v
        } else if let v = field("data:", line) {
            dataBuffer.append(contentsOf: v.utf8)
            dataBuffer.append(0x0A) // SSE joins multiple data lines with \n
        }
    }

    // field returns the value of an SSE field line, dropping the prefix and one
    // optional leading space, or nil when the prefix doesn't match.
    private func field(_ prefix: String, _ line: String) -> String? {
        guard line.hasPrefix(prefix) else { return nil }
        let rest = line.dropFirst(prefix.count)
        return rest.hasPrefix(" ") ? String(rest.dropFirst()) : String(rest)
    }

    private func dispatchFrame() {
        defer {
            eventName = ""
            dataBuffer.removeAll(keepingCapacity: true)
        }
        guard !eventName.isEmpty, !dataBuffer.isEmpty else { return }
        var d = dataBuffer
        if d.last == 0x0A { d.removeLast() } // drop the trailing join newline
        onEvent(eventName, d)
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        guard !stopped else { return }
        let delay = backoff
        backoff = min(backoff * 2, maxBackoff)
        Log.debug("EventStream disconnected (\(error?.localizedDescription ?? "EOF")); reconnecting in \(delay)s")
        MainThread.soon { [weak self] in
            after(delay) { [weak self] in
                guard let self = self, !self.stopped else { return }
                self.connect()
            }
        }
    }
}
