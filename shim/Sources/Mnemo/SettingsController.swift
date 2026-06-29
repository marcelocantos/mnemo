// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

// SettingsWindowController hosts the app's settings and status in a tabbed
// window (🎯T85, 🎯T86). The gear opens it on the Settings tab; the popover
// footer, the status-item menu, and a health notification's "Open Dashboard"
// action open it on the Status tab. Both tabs share one window so there is a
// single place for everything the app surfaces beyond the popover.
final class SettingsWindowController: NSWindowController, NSWindowDelegate {
    enum Tab: Int { case settings = 0, status = 1 }

    // A menu-bar (LSUIElement) app has nothing else retaining the controller
    // once the opener returns, so it keeps itself alive via this static
    // reference while the window is on screen, released in windowWillClose.
    private(set) static var current: SettingsWindowController?

    private let health: HealthController
    private let onChange: () -> Void
    private let pathField = NSTextField(labelWithString: "")
    private let tabView = NSTabView()
    private let dashboard = DashboardView()
    private var root = ""

    private init(health: HealthController, onChange: @escaping () -> Void) {
        self.health = health
        self.onChange = onChange
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 500, height: 600),
            styleMask: [.titled, .closable, .resizable],
            backing: .buffered, defer: false)
        window.title = "Mnemo"
        // ARC + NSWindowController own the window; don't let -close free it.
        window.isReleasedWhenClosed = false
        super.init(window: window)
        window.delegate = self
        build()
        fetchRoot()
        wireStatus()
    }

    required init?(coder: NSCoder) { fatalError("init(coder:) is unused") }

    // show presents the window on the requested tab, creating it on first use
    // and reusing (and re-seeding) an existing instance otherwise. health is the
    // single shared controller, so reuse is safe.
    static func show(health: HealthController, onChange: @escaping () -> Void, select: Tab) {
        let c = current ?? SettingsWindowController(health: health, onChange: onChange)
        current = c
        c.dashboard.update(health.currentReport())
        c.select(select)
        NSApp.activate(ignoringOtherApps: true)
        c.window?.center()
        c.window?.makeKeyAndOrderFront(nil)
    }

    private func select(_ tab: Tab) { tabView.selectTabViewItem(at: tab.rawValue) }

    // wireStatus seeds the dashboard and subscribes it to live health updates;
    // the subscription is torn down on close so a streamed report never touches
    // a dead view.
    private func wireStatus() {
        dashboard.update(health.currentReport())
        dashboard.onRefresh = { [weak self] in self?.health.refresh() }
        dashboard.onOpenFull = {
            if let url = DaemonClient.shared.dashboardURL() { NSWorkspace.shared.open(url) }
        }
        health.onReportUpdate = { [weak self] report in
            MainThread.soon { self?.dashboard.update(report) }
        }
    }

    private func build() {
        guard let content = window?.contentView else { return }

        tabView.translatesAutoresizingMaskIntoConstraints = false
        let settingsItem = NSTabViewItem(identifier: "settings")
        settingsItem.label = "Settings"
        settingsItem.view = buildSettingsTab()
        let statusItem = NSTabViewItem(identifier: "status")
        statusItem.label = "Status"
        statusItem.view = dashboard
        tabView.addTabViewItem(settingsItem)
        tabView.addTabViewItem(statusItem)
        content.addSubview(tabView)

        NSLayoutConstraint.activate([
            tabView.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 12),
            tabView.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -12),
            tabView.topAnchor.constraint(equalTo: content.topAnchor, constant: 12),
            tabView.bottomAnchor.constraint(equalTo: content.bottomAnchor, constant: -12),
        ])
    }

    private func buildSettingsTab() -> NSView {
        let container = NSView()

        // --- Setting: threads folder ---
        let heading = NSTextField(labelWithString: "Threads folder")
        heading.font = .boldSystemFont(ofSize: NSFont.systemFontSize)

        pathField.stringValue = displayPath(root)
        pathField.textColor = .secondaryLabelColor
        pathField.lineBreakMode = .byTruncatingMiddle
        pathField.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)

        let choose = NSButton(title: "Choose…", target: self, action: #selector(chooseFolder))
        choose.bezelStyle = .rounded
        choose.setContentHuggingPriority(.required, for: .horizontal)

        let pathRow = NSStackView(views: [pathField, choose])
        pathRow.orientation = .horizontal
        pathRow.alignment = .centerY
        pathRow.spacing = 8

        let folder = NSStackView(views: [heading, pathRow])
        folder.orientation = .vertical
        folder.alignment = .leading
        folder.spacing = 6

        // The vertical stack is the extension point: new setting rows insert
        // here as features land.
        let stack = NSStackView(views: [folder])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 18
        stack.translatesAutoresizingMaskIntoConstraints = false
        container.addSubview(stack)

        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: container.leadingAnchor, constant: 12),
            stack.trailingAnchor.constraint(equalTo: container.trailingAnchor, constant: -12),
            stack.topAnchor.constraint(equalTo: container.topAnchor, constant: 16),
            stack.bottomAnchor.constraint(lessThanOrEqualTo: container.bottomAnchor, constant: -12),
            folder.widthAnchor.constraint(equalTo: stack.widthAnchor),
            pathRow.widthAnchor.constraint(equalTo: folder.widthAnchor),
        ])
        return container
    }

    // fetchRoot pulls the current threads folder from the daemon so the Settings
    // tab is self-contained regardless of which entry point opened the window.
    private func fetchRoot() {
        DaemonClient.shared.threadsRoot { [weak self] root in
            MainThread.soon {
                guard let self = self else { return }
                self.root = root
                self.pathField.stringValue = self.displayPath(root)
            }
        }
    }

    @objc private func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.canCreateDirectories = true // adds the New Folder button
        panel.prompt = "Use Folder"
        panel.message = "Choose the folder that contains your threads"
        if !root.isEmpty { panel.directoryURL = URL(fileURLWithPath: root) }
        guard let window = window else { return }
        panel.beginSheetModal(for: window) { [weak self] resp in
            guard resp == .OK, let url = panel.url, let self = self else { return }
            let path = url.path
            DaemonClient.shared.setThreadsRoot(path) { [weak self] _ in
                MainThread.soon {
                    guard let self = self else { return }
                    self.root = path
                    self.pathField.stringValue = self.displayPath(path)
                    self.onChange()
                }
            }
        }
    }

    func windowWillClose(_ notification: Notification) {
        // Stop pushing streamed reports into a view that's going away.
        health.onReportUpdate = nil
        if SettingsWindowController.current === self {
            SettingsWindowController.current = nil
        }
    }

    // snapshot renders the whole window (tab bar + selected tab) for
    // screencapture self-test verification.
    func snapshot(to path: String) {
        guard let view = window?.contentView,
              let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return }
        view.cacheDisplay(in: view.bounds, to: rep)
        guard let data = rep.representation(using: .png, properties: [:]) else { return }
        try? data.write(to: URL(fileURLWithPath: path))
        Log.debug("settings snapshot written \(path)")
    }

    // displayPath abbreviates the home directory to ~ for readability.
    private func displayPath(_ path: String) -> String {
        if path.isEmpty { return "(not set)" }
        return (path as NSString).abbreviatingWithTildeInPath
    }
}
