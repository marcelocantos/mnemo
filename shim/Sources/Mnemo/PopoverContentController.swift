// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import AppKit

// PopoverContentController is the popover's content: a left markdown-preview
// pane and a right sidebar (search field, scrollable thread list, pinned
// footer of action rows). It loads data from the daemon and forwards user
// intent back to it; it holds no business logic (§6, Integration §0.1).
final class PopoverContentController: NSViewController, NSTableViewDataSource, NSTableViewDelegate {
    private let preview = PreviewView()
    private let table = ThreadTableView()
    private let tableScroll = NSScrollView()
    private let searchField = NSSearchField()

    private var allThreads: [ThreadView] = []
    private var shown: [ThreadView] = []
    private var root = ""
    private var hovered = -1
    private var previewCache: [String: String] = [:]
    private var markerCatalog: [MarkerInfo] = []
    private var pluginContribs: [PluginUIContribution] = []
    private var pluginFooterHost: NSView?
    private var viewingPluginName: String?
    private var keyMonitor: Any?
    private var refreshGeneration = 0
    // onRequestClose asks the owner (StatusItemController) to close the popover,
    // so all dismissal goes through one path (NSPopover.performClose).
    var onRequestClose: (() -> Void)?
    // onOpenDashboard / onOpenSettings ask the owner to show the settings window
    // on the Status / Settings tab (🎯T86); the owner dismisses the popover as
    // part of opening it.
    var onOpenDashboard: (() -> Void)?
    var onOpenSettings: (() -> Void)?

    private let previewWidth: CGFloat = 540
    private let markerWidth: CGFloat = 20
    private let countWidth: CGFloat = 26 // active / overdue todo columns
    private let ageWidth: CGFloat = 48
    // An empty trailing column keeps the last data column clear of the popover
    // edge while the table (and its header border) still spans full width —
    // insetting the table instead cut the header's bottom border short.
    private let spacerWidth: CGFloat = 14
    // The sidebar fits: marker | name | age | active | overdue | spacer.
    private let sidebarWidth: CGFloat = 320
    // Popover content height (50% taller than the original 460pt).
    private let popupHeight: CGFloat = 690
    // Shared height for thread rows and footer rows — the midpoint between the
    // previously too-tall thread rows and too-cramped footer, so the two read
    // as one list.
    private let rowHeight: CGFloat = 26

    private var dark: Bool {
        view.effectiveAppearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
    }

    // MARK: - view construction

    override func loadView() {
        let root = NSView(frame: NSRect(x: 0, y: 0, width: previewWidth + sidebarWidth, height: popupHeight))

        preview.translatesAutoresizingMaskIntoConstraints = false
        root.addSubview(preview)

        let sidebar = buildSidebar()
        sidebar.translatesAutoresizingMaskIntoConstraints = false
        root.addSubview(sidebar)

        NSLayoutConstraint.activate([
            preview.topAnchor.constraint(equalTo: root.topAnchor),
            preview.bottomAnchor.constraint(equalTo: root.bottomAnchor),
            preview.leadingAnchor.constraint(equalTo: root.leadingAnchor),
            preview.widthAnchor.constraint(equalToConstant: previewWidth),

            sidebar.topAnchor.constraint(equalTo: root.topAnchor),
            sidebar.bottomAnchor.constraint(equalTo: root.bottomAnchor),
            sidebar.leadingAnchor.constraint(equalTo: preview.trailingAnchor),
            sidebar.trailingAnchor.constraint(equalTo: root.trailingAnchor),
            sidebar.widthAnchor.constraint(equalToConstant: sidebarWidth),
        ])

        // Setting a fixed content size once is correct; never setFrame on the
        // popover window after it shows (§6).
        preferredContentSize = root.frame.size
        view = root
    }

    private func buildSidebar() -> NSView {
        let container = NSView()

        searchField.placeholderString = "Search threads"
        searchField.translatesAutoresizingMaskIntoConstraints = false
        searchField.target = self
        searchField.action = #selector(searchChanged)
        searchField.sendAction(on: .keyUp)
        container.addSubview(searchField)

        // Settings gear, to the right of the search field.
        let gear = NSButton()
        gear.image = NSImage(systemSymbolName: "gearshape", accessibilityDescription: "Settings")
        gear.imagePosition = .imageOnly
        gear.isBordered = false
        gear.bezelStyle = .regularSquare
        gear.contentTintColor = .secondaryLabelColor
        gear.toolTip = "Settings"
        gear.target = self
        gear.action = #selector(showSettings)
        gear.translatesAutoresizingMaskIntoConstraints = false
        container.addSubview(gear)

        configureTable()
        tableScroll.translatesAutoresizingMaskIntoConstraints = false
        tableScroll.documentView = table
        tableScroll.hasVerticalScroller = true
        tableScroll.drawsBackground = false
        tableScroll.automaticallyAdjustsContentInsets = false
        container.addSubview(tableScroll)

        let footer = buildFooter()
        footer.translatesAutoresizingMaskIntoConstraints = false
        container.addSubview(footer)

        NSLayoutConstraint.activate([
            searchField.topAnchor.constraint(equalTo: container.topAnchor, constant: 8),
            searchField.leadingAnchor.constraint(equalTo: container.leadingAnchor, constant: 8),
            searchField.trailingAnchor.constraint(equalTo: gear.leadingAnchor, constant: -6),

            gear.trailingAnchor.constraint(equalTo: container.trailingAnchor, constant: -8),
            gear.centerYAnchor.constraint(equalTo: searchField.centerYAnchor),
            gear.widthAnchor.constraint(equalToConstant: 22),

            tableScroll.topAnchor.constraint(equalTo: searchField.bottomAnchor, constant: 6),
            tableScroll.leadingAnchor.constraint(equalTo: container.leadingAnchor),
            tableScroll.trailingAnchor.constraint(equalTo: container.trailingAnchor),
            tableScroll.bottomAnchor.constraint(equalTo: footer.topAnchor),

            footer.leadingAnchor.constraint(equalTo: container.leadingAnchor),
            footer.trailingAnchor.constraint(equalTo: container.trailingAnchor),
            footer.bottomAnchor.constraint(equalTo: container.bottomAnchor),
        ])
        return container
    }

    private func configureTable() {
        table.style = .fullWidth
        table.selectionHighlightStyle = .none
        // A heading row labels the columns with emojis.
        table.headerView = NSTableHeaderView()
        table.backgroundColor = .clear
        table.rowHeight = rowHeight
        // Remove the default horizontal gap between columns so the marker sits
        // close to the name.
        table.intercellSpacing = NSSize(width: 0, height: 2)
        table.dataSource = self
        table.delegate = self
        table.onHoverRow = { [weak self] row in self?.hover(row) }
        table.contextMenuForRow = { [weak self] row in self?.markerMenu(row) }
        // A single click activates a row. Using the table's target/action
        // (clickedRow) is far more reliable than a custom mouseDown, which a
        // cell view can intercept before it reaches the table.
        table.target = self
        table.action = #selector(rowClicked)

        func addColumn(_ id: String, width: CGFloat, header: String) {
            let col = NSTableColumn(identifier: .init(id))
            col.width = width
            col.headerCell.stringValue = header
            col.headerCell.alignment = .center
            table.addTableColumn(col)
        }
        let nameWidth = sidebarWidth - markerWidth - ageWidth - countWidth * 2 - spacerWidth
        addColumn("marker", width: markerWidth, header: "")
        addColumn("name", width: nameWidth, header: "")
        addColumn("age", width: ageWidth, header: "🕘")
        addColumn("active", width: countWidth, header: "📋")
        addColumn("overdue", width: countWidth, header: "⏰")
        addColumn("spacer", width: spacerWidth, header: "")
    }

    private func buildFooter() -> NSView {
        let container = NSView()
        // Host for dynamic plugin rows (🎯T102.9); rebuilt when /api/plugins changes.
        let pluginHost = NSView()
        pluginHost.translatesAutoresizingMaskIntoConstraints = false
        container.addSubview(pluginHost)
        pluginFooterHost = pluginHost

        let staticBox = NSView()
        staticBox.translatesAutoresizingMaskIntoConstraints = false
        container.addSubview(staticBox)

        let items: [(String, () -> Void)] = [
            ("New thread…", { [weak self] in self?.newThread() }),
            ("Dashboard…", { [weak self] in self?.onOpenDashboard?() }),
            ("Show threads in Finder", { [weak self] in self?.openRoot() }),
        ]
        var prev: NSView?
        for (title, action) in items {
            let row = HoverRow(title: title, action: action, onHover: { [weak self] entered in
                // Hovering a footer row clears the thread hover + preview (§6).
                if entered { self?.hover(-1) }
            })
            row.translatesAutoresizingMaskIntoConstraints = false
            staticBox.addSubview(row)
            NSLayoutConstraint.activate([
                row.leadingAnchor.constraint(equalTo: staticBox.leadingAnchor),
                row.trailingAnchor.constraint(equalTo: staticBox.trailingAnchor),
                row.heightAnchor.constraint(equalToConstant: rowHeight),
                row.topAnchor.constraint(
                    equalTo: prev?.bottomAnchor ?? staticBox.topAnchor, constant: prev == nil ? 4 : 0),
            ])
            prev = row
        }
        prev?.bottomAnchor.constraint(equalTo: staticBox.bottomAnchor, constant: -6).isActive = true

        NSLayoutConstraint.activate([
            pluginHost.topAnchor.constraint(equalTo: container.topAnchor),
            pluginHost.leadingAnchor.constraint(equalTo: container.leadingAnchor),
            pluginHost.trailingAnchor.constraint(equalTo: container.trailingAnchor),

            staticBox.topAnchor.constraint(equalTo: pluginHost.bottomAnchor),
            staticBox.leadingAnchor.constraint(equalTo: container.leadingAnchor),
            staticBox.trailingAnchor.constraint(equalTo: container.trailingAnchor),
            staticBox.bottomAnchor.constraint(equalTo: container.bottomAnchor),
        ])
        // Empty host has zero height until plugins load.
        pluginHost.heightAnchor.constraint(greaterThanOrEqualToConstant: 0).isActive = true
        return container
    }

    // rebuildPluginFooter fills pluginFooterHost from the latest /api/plugins list.
    private func rebuildPluginFooter() {
        guard let host = pluginFooterHost else { return }
        host.subviews.forEach { $0.removeFromSuperview() }
        var prev: NSView?
        for p in pluginContribs {
            let title = p.label.isEmpty ? p.name : p.label
            let row = HoverRow(title: "  \(title)", action: { [weak self] in
                self?.openPluginPage(p)
            }, onHover: { [weak self] entered in
                if entered {
                    self?.hover(-1)
                    self?.showPluginPreview(p)
                }
            })
            row.translatesAutoresizingMaskIntoConstraints = false
            host.addSubview(row)
            NSLayoutConstraint.activate([
                row.leadingAnchor.constraint(equalTo: host.leadingAnchor),
                row.trailingAnchor.constraint(equalTo: host.trailingAnchor),
                row.heightAnchor.constraint(equalToConstant: rowHeight),
                row.topAnchor.constraint(
                    equalTo: prev?.bottomAnchor ?? host.topAnchor, constant: prev == nil ? 4 : 0),
            ])
            prev = row
        }
        if let prev = prev {
            prev.bottomAnchor.constraint(equalTo: host.bottomAnchor).isActive = true
        } else {
            host.heightAnchor.constraint(equalToConstant: 0).isActive = true
        }
    }

    private func showPluginPreview(_ p: PluginUIContribution) {
        viewingPluginName = p.name
        guard let path = p.previewURL ?? p.pageURL,
              let url = DaemonClient.shared.absoluteURL(path) else {
            preview.showPlain("Plugin \(p.name) has no preview URL")
            return
        }
        preview.loadURL(url)
    }

    private func openPluginPage(_ p: PluginUIContribution) {
        let path = p.pageURL ?? p.previewURL
        guard let path, let url = DaemonClient.shared.absoluteURL(path) else { return }
        NSWorkspace.shared.open(url)
    }

    // handlePluginReload reacts to the plugin.reload SSE event (🎯T102.9).
    func handlePluginReload(name: String?) {
        if let name = name, let p = pluginContribs.first(where: { $0.name == name }) {
            if viewingPluginName == name {
                preview.reload()
            }
            // Refresh catalog in case label/URLs changed.
            _ = p
        } else if viewingPluginName != nil {
            preview.reload()
        }
        fetchPlugins()
    }

    private func fetchPlugins() {
        DaemonClient.shared.plugins { [weak self] result in
            MainThread.soon {
                guard let self = self else { return }
                if case .success(let list) = result {
                    self.pluginContribs = list
                    self.rebuildPluginFooter()
                }
            }
        }
    }

    // MARK: - lifecycle hooks

    // prewarm loads the list and pre-fetches each preview so the first hover is
    // instant (§6 "Prewarming").
    func prewarm() {
        _ = view
        fetchMarkerCatalog()
        fetchPlugins()
        refresh { [weak self] in self?.prefetchPreviews() }
    }

    // fetchMarkerCatalog loads the daemon's marker vocabulary once, so the
    // right-click menu is built from it rather than hardcoded in the shim.
    private func fetchMarkerCatalog() {
        DaemonClient.shared.markers { [weak self] result in
            MainThread.soon {
                if case .success(let cat) = result { self?.markerCatalog = cat }
            }
        }
    }

    func popoverDidOpen() {
        _ = view
        // Drop cached previews so edits to a thread's CLAUDE.md show on the next
        // open (the cache is otherwise per-session; mtime-precise invalidation
        // would need the daemon to surface the file mtime).
        previewCache.removeAll()
        viewingPluginName = nil
        fetchPlugins()
        refresh(nil)
        installKeyMonitor()
        MainThread.soon { [weak self] in
            guard let self = self else { return }
            // Belt-and-suspenders for hover: the table's tracking area drives
            // mouseMoved, but enabling window mouse-moved events too avoids any
            // gap while the transient popover is non-key.
            self.view.window?.acceptsMouseMovedEvents = true
            self.view.window?.makeFirstResponder(self.searchField)
            Log.debug("popoverDidOpen key=\(self.view.window?.isKeyWindow ?? false) acceptsMoved=\(self.view.window?.acceptsMouseMovedEvents ?? false) rows=\(self.shown.count)")
        }
    }

    // MARK: - data

    private func refresh(_ then: (() -> Void)?) {
        refreshGeneration += 1
        let gen = refreshGeneration
        DaemonClient.shared.list { [weak self] result in
            MainThread.soon {
                guard let self = self, gen == self.refreshGeneration else { return }
                if case .success(let list) = result {
                    self.allThreads = list.threads
                    self.root = list.root
                    self.applyFilter()
                }
                then?()
            }
        }
    }

    private func prefetchPreviews() {
        for t in allThreads { fetchPreview(t.name) { _ in } }
    }

    private func fetchPreview(_ name: String, _ done: @escaping (String) -> Void) {
        let key = "\(name)|\(dark ? "d" : "l")"
        if let cached = previewCache[key] {
            Log.debug("fetchPreview \(name) cache hit len=\(cached.count)")
            done(cached); return
        }
        Log.debug("fetchPreview \(name) cache miss; requesting")
        DaemonClient.shared.preview(name: name, dark: dark) { [weak self] result in
            MainThread.soon {
                guard let self = self else { return }
                switch result {
                case .success(let html):
                    Log.debug("fetchPreview \(name) got len=\(html.count)")
                    self.previewCache[key] = html
                    done(html)
                case .failure(let err):
                    Log.debug("fetchPreview \(name) FAILED: \(err)")
                }
            }
        }
    }

    // MARK: - filtering (§6 instant channel)

    @objc private func searchChanged() { applyFilter() }

    private func applyFilter() {
        let needle = searchField.stringValue.trimmingCharacters(in: .whitespaces).lowercased()
        if needle.isEmpty {
            shown = allThreads
        } else {
            shown = allThreads.filter { $0.name.lowercased().contains(needle) }
            // Deep channel: ask the daemon for threads whose content matches
            // and union them in when they return (broadening the set).
            deepSearch(needle)
        }
        table.reloadData()
        if hovered >= shown.count { hover(shown.isEmpty ? -1 : 0) }
    }

    private func deepSearch(_ needle: String) {
        let gen = refreshGeneration
        DaemonClient.shared.search(query: needle) { [weak self] names in
            MainThread.soon {
                // Generation-counted so a stale async reply is dropped (§6).
                guard let self = self, gen == self.refreshGeneration else { return }
                let matched = Set(names)
                let have = Set(self.shown.map { $0.name })
                let extra = self.allThreads.filter { matched.contains($0.name) && !have.contains($0.name) }
                guard !extra.isEmpty else { return }
                self.shown.append(contentsOf: extra)
                self.table.reloadData()
            }
        }
    }

    // MARK: - interaction

    private func hover(_ row: Int) {
        guard row != hovered else { return }
        Log.debug("hover \(row)")
        hovered = row
        viewingPluginName = nil
        table.enumerateAvailableRowViews { rowView, idx in
            rowView.backgroundColor = (idx == row)
                ? NSColor.selectedContentBackgroundColor.withAlphaComponent(0.25)
                : .clear
        }
        if row < 0 || row >= shown.count {
            preview.clear()
            return
        }
        fetchPreview(shown[row].name) { [weak self] html in self?.preview.showHTML(html) }
    }

    @objc private func rowClicked() {
        Log.debug("rowClicked clickedRow=\(table.clickedRow)")
        activate(table.clickedRow)
    }

    // selfTestHoverFirst / selfTestActivateFirst drive the first row from code
    // for screencapture-based verification (see StatusItemController.selfTest).
    func selfTestHoverFirst() {
        Log.debug("selfTest: hovering first of \(shown.count)")
        if !shown.isEmpty { hover(0) }
    }

    func selfTestActivateFirst() {
        Log.debug("selfTest: activating first of \(shown.count)")
        if !shown.isEmpty { activate(0) }
    }

    // snapshot renders the popover content to a PNG, independent of window
    // focus (so a transient popover that would dismiss under an external
    // screencapture is still captured). Used by the self-test driver.
    func snapshot(to path: String) {
        _ = view
        let target = view
        guard let rep = target.bitmapImageRepForCachingDisplay(in: target.bounds) else {
            Log.debug("snapshot: no bitmap rep")
            return
        }
        target.cacheDisplay(in: target.bounds, to: rep)
        guard let data = rep.representation(using: .png, properties: [:]) else {
            Log.debug("snapshot: png encode failed")
            return
        }
        do {
            try data.write(to: URL(fileURLWithPath: path))
            Log.debug("snapshot written \(path) bounds=\(target.bounds)")
        } catch {
            Log.debug("snapshot write failed: \(error)")
        }
    }

    private func activate(_ row: Int) {
        guard row >= 0, row < shown.count else { return }
        let name = shown[row].name
        Log.debug("activate \(name)")
        dismiss()
        DaemonClient.shared.go(name: name, noResume: false) { result in
            if case .failure(let err) = result { Log.debug("go FAILED: \(err)") }
        }
    }

    // markerMenu is the right-click menu for a row, built data-drivenly from the
    // daemon's marker catalog (no hardcoded vocabulary): every marker except the
    // current one is offered as a "Set as …" action, so a marker added
    // daemon-side appears here with no shim change.
    private func markerMenu(_ row: Int) -> NSMenu? {
        guard row >= 0, row < shown.count else { return nil }
        let t = shown[row]
        let current = t.marker ?? ""
        let menu = NSMenu()
        for mi in markerCatalog where mi.value != current {
            let item = NSMenuItem(
                title: "Set as \(mi.label) \(mi.emoji)",
                action: #selector(setMarkerAction(_:)), keyEquivalent: "")
            item.target = self
            item.representedObject = ["name": t.name, "marker": mi.value]
            menu.addItem(item)
        }
        return menu.items.isEmpty ? nil : menu
    }

    @objc private func setMarkerAction(_ sender: NSMenuItem) {
        guard let info = sender.representedObject as? [String: String],
              let name = info["name"], let marker = info["marker"] else { return }
        DaemonClient.shared.setMarker(name: name, marker: marker) { [weak self] _ in
            MainThread.soon { self?.refresh(nil) }
        }
    }

    // MARK: - footer actions

    @objc private func newThread() {
        let alert = NSAlert()
        alert.messageText = "New thread"
        alert.informativeText = "Enter a kebab-case name:"
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 220, height: 24))
        alert.accessoryView = field
        alert.addButton(withTitle: "Create")
        alert.addButton(withTitle: "Cancel")
        NSApp.activate(ignoringOtherApps: true)
        // Focus the text field, not a button, when the dialog appears.
        alert.window.initialFirstResponder = field
        if alert.runModal() == .alertFirstButtonReturn {
            let name = field.stringValue.trimmingCharacters(in: .whitespaces)
            guard !name.isEmpty else { return }
            DaemonClient.shared.create(name: name) { [weak self] _ in
                MainThread.soon { self?.refresh(nil) }
                DaemonClient.shared.go(name: name, noResume: false) { _ in }
            }
        }
    }

    @objc private func openRoot() {
        if !root.isEmpty {
            NSWorkspace.shared.open(URL(fileURLWithPath: root))
        }
        dismiss()
    }

    // showSettings (the gear) asks the owner to open the settings window on the
    // Settings tab; the owner dismisses the transient popover so it doesn't
    // compete with the settings window for key focus.
    @objc private func showSettings() {
        onOpenSettings?()
    }

    // reload re-fetches the thread list (e.g. after the threads folder changes
    // in settings).
    func reload() { refresh(nil) }

    private func dismiss() {
        onRequestClose?()
    }

    // popoverWillClose is called by the owner when the popover closes by any
    // path, so the key monitor never leaks past a dismissal.
    func popoverWillClose() {
        removeKeyMonitor()
    }

    // MARK: - keyboard nav (§6)

    private func installKeyMonitor() {
        removeKeyMonitor()
        keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] event in
            guard let self = self else { return event }
            switch event.keyCode {
            case 125: self.moveHover(1); return nil   // ↓
            case 126: self.moveHover(-1); return nil  // ↑
            case 36: // Return
                if self.hovered >= 0 { self.activate(self.hovered) }
                return nil
            case 53: // Esc
                if self.searchField.stringValue.isEmpty {
                    self.dismiss()
                } else {
                    self.searchField.stringValue = ""
                    self.applyFilter()
                }
                return nil
            default:
                return event
            }
        }
    }

    private func removeKeyMonitor() {
        if let m = keyMonitor { NSEvent.removeMonitor(m) }
        keyMonitor = nil
    }

    private func moveHover(_ delta: Int) {
        guard !shown.isEmpty else { return }
        var next = hovered + delta
        next = max(0, min(shown.count - 1, next))
        hover(next)
        table.scrollRowToVisible(next)
    }

    // MARK: - NSTableViewDataSource / Delegate

    func numberOfRows(in tableView: NSTableView) -> Int { shown.count }

    func tableView(_ tableView: NSTableView, viewFor tableColumn: NSTableColumn?, row: Int) -> NSView? {
        guard row < shown.count, let col = tableColumn else { return nil }
        let t = shown[row]
        let id = col.identifier
        // An NSTableCellView container forwards mouse events to the table (so
        // hover tracking and the click action both fire); a bare NSTextField
        // would intercept them.
        let cell = (tableView.makeView(withIdentifier: id, owner: self) as? NSTableCellView) ?? makeCell(id)
        guard let label = cell.textField else { return cell }
        switch id.rawValue {
        case "marker":
            label.stringValue = t.glyph
            label.alignment = .right
        case "name":
            label.stringValue = t.name
            label.textColor = .labelColor
            label.alignment = .left
        case "active":
            label.stringValue = t.activeText
            label.textColor = .secondaryLabelColor
            label.alignment = .center
        case "overdue":
            label.stringValue = t.overdueText
            label.textColor = .systemRed
            label.alignment = .center
        case "age":
            label.stringValue = t.age
            label.textColor = .secondaryLabelColor
            label.alignment = .center
        default: // spacer
            label.stringValue = ""
        }
        return cell
    }

    private func makeCell(_ id: NSUserInterfaceItemIdentifier) -> NSTableCellView {
        let cell = NSTableCellView()
        cell.identifier = id
        let label = NSTextField(labelWithString: "")
        label.lineBreakMode = .byTruncatingTail
        label.translatesAutoresizingMaskIntoConstraints = false
        cell.addSubview(label)
        cell.textField = label
        // The marker glyph hugs both edges (it's right-aligned via the cell so
        // it sits next to the name); the name/age get a small left inset.
        let inset: CGFloat = id.rawValue == "marker" ? 0 : 2
        NSLayoutConstraint.activate([
            label.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: inset),
            label.trailingAnchor.constraint(equalTo: cell.trailingAnchor, constant: -inset),
            label.centerYAnchor.constraint(equalTo: cell.centerYAnchor),
        ])
        return cell
    }
}
