// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit
import WebKit

// PreviewView renders the daemon's preview HTML in a WKWebView — a true browser
// view of GET /api/thread/preview. The daemon owns markdown→HTML and theming
// (full inline CSS including the body background), so the shim adds no rendering
// logic: it just loads the finished HTML.
//
// This replaced an NSTextView + NSAttributedString(html:) path whose limited
// HTML/CSS support dropped the body background (forcing a hardcoded palette that
// duplicated render.go) and injected stray list attributes (forcing a strip
// pass), and needed TextKit-2 / responsive-scrolling / flipped-clip workarounds.
// A WebView renders the daemon's CSS faithfully and needs none of that. The one
// historical objection — "a WebView content process won't launch outside a .app
// bundle" — was resolved when the shim shipped as Mnemo.app (T85.5).
final class PreviewView: NSView {
    private let webView: WKWebView
    private let nav = PreviewNavigationDelegate()

    override init(frame frameRect: NSRect) {
        webView = WKWebView(frame: frameRect, configuration: WKWebViewConfiguration())
        super.init(frame: frameRect)

        // Transparent until the HTML paints, so the popover material shows
        // through rather than a white flash; the daemon's themed body background
        // paints on top. (drawsBackground has no public setter on macOS.)
        webView.setValue(false, forKey: "drawsBackground")
        webView.navigationDelegate = nav
        webView.translatesAutoresizingMaskIntoConstraints = false
        addSubview(webView)
        NSLayoutConstraint.activate([
            webView.topAnchor.constraint(equalTo: topAnchor),
            webView.bottomAnchor.constraint(equalTo: bottomAnchor),
            webView.leadingAnchor.constraint(equalTo: leadingAnchor),
            webView.trailingAnchor.constraint(equalTo: trailingAnchor),
        ])
    }

    required init?(coder: NSCoder) { fatalError("not used") }

    // showHTML displays the daemon's finished HTML. No theme argument: the HTML
    // is already theme-rendered server-side and carries its own palette.
    func showHTML(_ html: String) {
        webView.loadHTMLString(html, baseURL: nil)
    }

    func showPlain(_ text: String) {
        let escaped = text
            .replacingOccurrences(of: "&", with: "&amp;")
            .replacingOccurrences(of: "<", with: "&lt;")
        showHTML("<!DOCTYPE html><meta charset=\"utf-8\">"
            + "<body style=\"font:13px -apple-system;color:#888;margin:8px\">\(escaped)</body>")
    }

    func clear() { showHTML("") }
}

// PreviewNavigationDelegate keeps the preview a viewer, not a browser: the
// initial loadHTMLString proceeds, but a clicked link opens in the user's
// default browser instead of navigating the preview to an arbitrary page.
private final class PreviewNavigationDelegate: NSObject, WKNavigationDelegate {
    func webView(_ webView: WKWebView,
                 decidePolicyFor navigationAction: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if navigationAction.navigationType == .linkActivated,
           let url = navigationAction.request.url {
            NSWorkspace.shared.open(url)
            decisionHandler(.cancel)
            return
        }
        decisionHandler(.allow)
    }
}
