import AppKit

struct WorkerLogEntry: Decodable, Equatable {
    let id: String
    let time: String
    let level: String
    let home: String
    let message: String
}

struct WorkerLogPage {
    let entries: [WorkerLogEntry]
    let cursor: String
    let hasMore: Bool
    let reset: String?
}

/// The reader's IDs identify complete records, including identical messages at
/// different file offsets. A rotation/gap never silently discards what the
/// person was reading. The local history is bounded independently of each page.
struct WorkerLogHistory {
    private(set) var entries: [WorkerLogEntry] = []
    private(set) var cursor = ""
    private(set) var boundary: String?
    private(set) var discarded = 0
    private(set) var loaded = false
    private var ids: Set<String> = []
    var maximumEntries = 2000
    var maximumCharacters = 2 * 1024 * 1024

    mutating func accept(_ page: WorkerLogPage) {
        loaded = true
        cursor = page.cursor
        if let reset = page.reset {
            switch reset {
            case "rotated": boundary = "Log rotated. Earlier entries are kept."
            case "truncated": boundary = "Log restarted. Earlier entries are kept."
            case "expired", "oversized": boundary = "Some earlier entries are unavailable."
            case "missing": boundary = "Waiting for the worker log. Earlier entries are kept."
            default: boundary = "Log history changed. Earlier entries are kept."
            }
        } else if boundary?.hasPrefix("Waiting for") == true && !page.entries.isEmpty {
            boundary = nil
        }
        for entry in page.entries where ids.insert(entry.id).inserted { entries.append(entry) }
        var characters = entries.reduce(0) { $0 + $1.message.utf16.count + $1.time.utf16.count + $1.home.utf16.count + 32 }
        var remove = 0
        while entries.count - remove > maximumEntries || (characters > maximumCharacters && remove < entries.count) {
            let entry = entries[remove]
            characters -= entry.message.utf16.count + entry.time.utf16.count + entry.home.utf16.count + 32
            ids.remove(entry.id)
            remove += 1
        }
        if remove > 0 { entries.removeFirst(remove); discarded += remove }
    }

    func matching(search: String, level: String?, home: String?) -> [WorkerLogEntry] {
        let needle = search.trimmingCharacters(in: .whitespacesAndNewlines)
        return entries.filter { entry in
            (level == nil || entry.level == level) && (home == nil || entry.home == home) &&
                (needle.isEmpty || "\(entry.time) \(entry.level) \(entry.home) \(entry.message)".localizedCaseInsensitiveContains(needle))
        }
    }
}

/// Explicit text storage/layout keeps a long, changing log document's geometry
/// independent of viewport-only layout. Updates replace only the changed range;
/// an unchanged poll does not touch storage, selection, or the scroll position.
final class WorkerLogScrollView: NSScrollView {
    var willResize: (() -> Bool)?
    var didResize: ((Bool) -> Void)?
    override func resizeSubviews(withOldSize oldSize: NSSize) {
        let captured = willResize?() ?? false
        super.resizeSubviews(withOldSize: oldSize)
        didResize?(captured)
    }
}

final class WorkerLogDocument: NSObject, NSTextViewDelegate {
    let scrollView = WorkerLogScrollView()
    let textView: NSTextView
    var userScrolledAway: (() -> Void)?
    var followsTail = true
    private(set) var rendered: [WorkerLogEntry] = []
    private var ranges: [NSRange] = []
    private var changing = false
    private var observer: NSObjectProtocol?
    private var resizeAnchor: Anchor?
    private var resizeDistance: CGFloat = 0
    private var resizeOrigin: NSPoint = .zero
    private let parser = ISO8601DateFormatter()
    private let display: DateFormatter = {
        let formatter = DateFormatter(); formatter.dateFormat = "MMM d HH:mm:ss"; return formatter
    }()

    override init() {
        let storage = NSTextStorage()
        let layout = NSLayoutManager()
        let container = NSTextContainer(containerSize: NSSize(width: 900, height: CGFloat.greatestFiniteMagnitude))
        storage.addLayoutManager(layout); layout.addTextContainer(container)
        textView = NSTextView(frame: NSRect(x: 0, y: 0, width: 900, height: 480), textContainer: container)
        super.init()
        scrollView.hasVerticalScroller = true
        scrollView.hasHorizontalScroller = false
        scrollView.borderType = .noBorder
        scrollView.drawsBackground = true
        scrollView.backgroundColor = .textBackgroundColor
        textView.isEditable = false; textView.isSelectable = true
        textView.delegate = self
        textView.font = .monospacedSystemFont(ofSize: 12, weight: .regular)
        textView.textColor = .labelColor
        textView.backgroundColor = .textBackgroundColor
        textView.textContainerInset = NSSize(width: 12, height: 10)
        textView.minSize = .zero
        textView.maxSize = NSSize(width: CGFloat.greatestFiniteMagnitude, height: CGFloat.greatestFiniteMagnitude)
        textView.isHorizontallyResizable = false; textView.isVerticallyResizable = true
        textView.autoresizingMask = [.width]
        container.widthTracksTextView = true
        container.heightTracksTextView = false
        textView.setAccessibilityLabel("Worker log entries")
        scrollView.documentView = textView
        scrollView.willResize = { [weak self] in
            guard let self, !self.changing else { return false }
            self.changing = true
            let character = self.firstVisibleCharacter()
            self.resizeAnchor = self.anchor(character)
            self.resizeOrigin = self.scrollView.contentView.bounds.origin
            self.resizeDistance = self.resizeOrigin.y - self.characterRect(character).minY
            return true
        }
        scrollView.didResize = { [weak self] captured in
            guard let self, captured else { return }
            defer { self.changing = false }
            self.ensureDocumentLayout()
            if self.followsTail { self.scrollToBottom() }
            else { self.restoreViewport(self.resizeAnchor, distance: self.resizeDistance, fallback: self.resizeOrigin) }
        }
        scrollView.contentView.postsBoundsChangedNotifications = true
        observer = NotificationCenter.default.addObserver(forName: NSView.boundsDidChangeNotification,
            object: scrollView.contentView, queue: .main) { [weak self] _ in
                guard let self, !self.changing, !self.isAtBottom else { return }
                self.userScrolledAway?()
            }
    }

    deinit { if let observer { NotificationCenter.default.removeObserver(observer) } }

    var isAtBottom: Bool {
        scrollView.contentView.bounds.maxY >= textView.bounds.height - 4
    }
    var visibleText: String { rendered.isEmpty ? "" : textView.string }

    func textViewDidChangeSelection(_ notification: Notification) {
        if !changing && textView.selectedRanges.contains(where: { $0.rangeValue.length > 0 }) {
            userScrolledAway?()
        }
    }

    private struct Anchor {
        let id: String
        let offset: Int
    }
    private func anchor(_ position: Int) -> Anchor? {
        guard !rendered.isEmpty else { return nil }
        let index = ranges.firstIndex(where: { position < NSMaxRange($0) }) ?? (ranges.count - 1)
        return Anchor(id: rendered[index].id, offset: max(0, position - ranges[index].location))
    }
    private func position(_ anchor: Anchor?) -> Int? {
        guard let anchor, let index = rendered.firstIndex(where: { $0.id == anchor.id }) else { return nil }
        return ranges[index].location + min(anchor.offset, ranges[index].length)
    }
    private func characterRect(_ position: Int) -> NSRect {
        guard let layout = textView.layoutManager, let container = textView.textContainer,
              textView.string.utf16.count > 0 else { return .zero }
        let character = min(position, textView.string.utf16.count - 1)
        let glyph = layout.glyphRange(forCharacterRange: NSRange(location: character, length: 1), actualCharacterRange: nil)
        return layout.boundingRect(forGlyphRange: glyph, in: container).offsetBy(dx: textView.textContainerOrigin.x, dy: textView.textContainerOrigin.y)
    }
    private func firstVisibleCharacter() -> Int {
        guard let layout = textView.layoutManager, let container = textView.textContainer, layout.numberOfGlyphs > 0 else { return 0 }
        let point = NSPoint(x: 0, y: max(0, scrollView.contentView.bounds.minY - textView.textContainerOrigin.y))
        let glyph = min(layout.glyphIndex(for: point, in: container), layout.numberOfGlyphs - 1)
        return layout.characterIndexForGlyph(at: glyph)
    }

    private func formatted(_ entry: WorkerLogEntry) -> NSAttributedString {
        parser.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        var date = parser.date(from: entry.time)
        if date == nil { parser.formatOptions = [.withInternetDateTime]; date = parser.date(from: entry.time) }
        let stamp = date.map(display.string) ?? entry.time
        let font = NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
        let header = "\(stamp)  \(entry.level.padding(toLength: 5, withPad: " ", startingAt: 0))  \(entry.home.isEmpty ? "worker" : entry.home)\n"
        let text = NSMutableAttributedString(string: header, attributes: [.font: font, .foregroundColor: NSColor.secondaryLabelColor])
        if let range = header.range(of: entry.level), !entry.level.isEmpty {
            let color: NSColor = entry.level == "ERROR" ? .systemRed : entry.level == "WARN" ? .systemOrange : .secondaryLabelColor
            text.addAttribute(.foregroundColor, value: color, range: NSRange(range, in: header))
        }
        text.append(NSAttributedString(string: entry.message + "\n\n", attributes: [.font: font, .foregroundColor: NSColor.labelColor]))
        return text
    }

    func render(_ entries: [WorkerLogEntry], following: Bool, emptyText: String) {
        followsTail = following
        if entries == rendered && (!entries.isEmpty || textView.string == emptyText) {
            if following { scrollToBottom() }
            return
        }
        changing = true
        defer { changing = false }
        let selection = textView.selectedRanges.map { value -> (Anchor?, Anchor?) in
            let range = value.rangeValue
            return (anchor(range.location), anchor(NSMaxRange(range)))
        }
        let oldOrigin = scrollView.contentView.bounds.origin
        let top = firstVisibleCharacter()
        let topAnchor = anchor(top)
        let distance = oldOrigin.y - characterRect(top).minY
        let pieces = entries.map(formatted)
        var prefix = 0
        while prefix < min(rendered.count, entries.count) && rendered[prefix] == entries[prefix] { prefix += 1 }
        var suffix = 0
        while suffix < min(rendered.count, entries.count) - prefix && rendered[rendered.count - suffix - 1] == entries[entries.count - suffix - 1] { suffix += 1 }
        let start = prefix < ranges.count ? ranges[prefix].location : textView.string.utf16.count
        let end = suffix > 0 ? ranges[ranges.count - suffix].location : textView.string.utf16.count
        let replacement = NSMutableAttributedString(string: "")
        if entries.isEmpty {
            replacement.append(NSAttributedString(string: emptyText, attributes: [.font: NSFont.systemFont(ofSize: 13), .foregroundColor: NSColor.secondaryLabelColor]))
        } else {
            for piece in pieces[prefix..<(pieces.count - suffix)] { replacement.append(piece) }
        }
        let replaceRange = rendered.isEmpty || entries.isEmpty ? NSRange(location: 0, length: textView.string.utf16.count) : NSRange(location: start, length: end - start)
        textView.textStorage?.replaceCharacters(in: replaceRange, with: replacement)
        rendered = entries
        var offset = 0
        ranges = pieces.map { piece in
            let range = NSRange(location: offset, length: piece.length); offset += piece.length; return range
        }
        ensureDocumentLayout()
        let restored = selection.compactMap { start, end -> NSValue? in
            guard let start = position(start), let end = position(end), end >= start else { return nil }
            return NSValue(range: NSRange(location: start, length: end - start))
        }
        textView.selectedRanges = restored.isEmpty ? [NSValue(range: NSRange(location: 0, length: 0))] : restored
        if following {
            scrollToBottom()
        } else {
            restoreViewport(topAnchor, distance: distance, fallback: oldOrigin)
        }
    }

    private func restoreViewport(_ anchor: Anchor?, distance: CGFloat, fallback: NSPoint) {
        let maximum = max(0, textView.bounds.height - scrollView.contentView.bounds.height)
        let y = position(anchor).map { characterRect($0).minY + distance } ?? fallback.y
        scrollView.contentView.scroll(to: NSPoint(x: 0, y: min(maximum, max(0, y))))
        scrollView.reflectScrolledClipView(scrollView.contentView)
    }

    func ensureDocumentLayout() {
        guard let container = textView.textContainer, let layout = textView.layoutManager else { return }
        let width = max(1, scrollView.contentSize.width)
        textView.setFrameSize(NSSize(width: width, height: max(1, textView.frame.height)))
        container.containerSize = NSSize(width: max(1, width - 2 * textView.textContainerInset.width), height: CGFloat.greatestFiniteMagnitude)
        layout.ensureLayout(for: container)
        let height = max(scrollView.contentSize.height, ceil(layout.usedRect(for: container).height) + 2 * textView.textContainerInset.height)
        textView.setFrameSize(NSSize(width: width, height: height))
        textView.needsDisplay = true
    }

    func scrollToBottom() {
        let wasChanging = changing; changing = true
        defer { changing = wasChanging }
        ensureDocumentLayout()
        scrollView.contentView.scroll(to: NSPoint(x: 0, y: max(0, textView.bounds.height - scrollView.contentView.bounds.height)))
        scrollView.reflectScrolledClipView(scrollView.contentView)
    }
}

final class WorkerLogsWindow: NSWindowController, NSSearchFieldDelegate {
    var read: ((String, @escaping (WorkerLogPage?, String?) -> Void) -> Void)?
    let logDocument = WorkerLogDocument()
    let search = NSSearchField()
    let homeFilter = NSPopUpButton()
    let levelFilter = NSPopUpButton()
    let followButton = NSButton()
    let refreshButton = NSButton()
    let copyButton = NSButton()
    let summary = NSTextField(labelWithString: "")
    private(set) var history = WorkerLogHistory()
    private(set) var following = true
    private(set) var loading = false
    private(set) var failure: String?

    init() {
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 960, height: 580), styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        super.init(window: window)
        window.title = "MemQL — Worker Logs"
        window.minSize = NSSize(width: 650, height: 360)
        window.isReleasedWhenClosed = false
        let content = window.contentView!
        search.placeholderString = "Search"; search.delegate = self
        search.setAccessibilityLabel("Search worker logs")
        search.setContentHuggingPriority(.defaultLow, for: .horizontal)
        homeFilter.addItem(withTitle: "All servers")
        homeFilter.setAccessibilityLabel("Filter logs by server")
        levelFilter.addItems(withTitles: ["All levels", "INFO", "WARN", "ERROR", "DEBUG"])
        levelFilter.setAccessibilityLabel("Filter logs by level")
        for button in [homeFilter, levelFilter] { button.target = self; button.action = #selector(filtersChanged) }
        copyButton.image = NSImage(systemSymbolName: "doc.on.doc", accessibilityDescription: "Copy visible logs")
        copyButton.imagePosition = .imageOnly; copyButton.bezelStyle = .rounded
        copyButton.toolTip = "Copy visible logs"; copyButton.setAccessibilityLabel("Copy visible logs")
        copyButton.target = self; copyButton.action = #selector(copyLogs)
        refreshButton.image = NSImage(systemSymbolName: "arrow.clockwise", accessibilityDescription: "Refresh logs")
        refreshButton.imagePosition = .imageOnly; refreshButton.bezelStyle = .rounded
        refreshButton.toolTip = "Refresh logs"; refreshButton.setAccessibilityLabel("Refresh logs")
        refreshButton.target = self; refreshButton.action = #selector(refresh)
        followButton.title = "Pause follow"; followButton.bezelStyle = .rounded
        followButton.target = self; followButton.action = #selector(toggleFollow)
        followButton.setAccessibilityLabel("Pause following worker logs")
        summary.font = .systemFont(ofSize: 11); summary.textColor = .secondaryLabelColor
        summary.lineBreakMode = .byTruncatingTail
        summary.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        let toolbar = NSStackView(views: [search, homeFilter, levelFilter, copyButton])
        toolbar.spacing = 8; toolbar.orientation = .horizontal
        let footer = NSStackView(views: [summary, refreshButton, followButton])
        footer.spacing = 8; footer.orientation = .horizontal
        summary.setContentHuggingPriority(.defaultLow, for: .horizontal)
        for view in [toolbar, logDocument.scrollView, footer] { view.translatesAutoresizingMaskIntoConstraints = false; content.addSubview(view) }
        NSLayoutConstraint.activate([
            toolbar.topAnchor.constraint(equalTo: content.topAnchor, constant: 12), toolbar.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 16), toolbar.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -16),
            search.widthAnchor.constraint(greaterThanOrEqualToConstant: 160), homeFilter.widthAnchor.constraint(lessThanOrEqualToConstant: 220),
            logDocument.scrollView.topAnchor.constraint(equalTo: toolbar.bottomAnchor, constant: 12), logDocument.scrollView.leadingAnchor.constraint(equalTo: content.leadingAnchor), logDocument.scrollView.trailingAnchor.constraint(equalTo: content.trailingAnchor),
            logDocument.scrollView.bottomAnchor.constraint(equalTo: footer.topAnchor, constant: -10),
            footer.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 16), footer.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -16), footer.bottomAnchor.constraint(equalTo: content.bottomAnchor, constant: -10),
        ])
        logDocument.userScrolledAway = { [weak self] in self?.pauseFollowing() }
        window.center()
        content.layoutSubtreeIfNeeded()
        render()
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    @objc func refresh() {
        guard window?.isVisible == true, !loading, let read else { return }
        loading = true; updateSummary()
        let previousCursor = history.cursor
        read(previousCursor) { [weak self] page, error in
            guard let self else { return }
            self.loading = false
            if let page, error == nil { self.history.accept(page); self.failure = nil }
            else { self.failure = error ?? "Worker logs are unavailable." }
            self.updateServers(); self.render()
            if let page, page.hasMore, page.cursor != previousCursor, error == nil, self.window?.isVisible == true {
                DispatchQueue.main.async { [weak self] in self?.refresh() }
            }
        }
    }

    private func updateServers() {
        let selected = homeFilter.titleOfSelectedItem ?? "All servers"
        var homes = Set(history.entries.map(\.home).filter { !$0.isEmpty })
        if selected != "All servers" { homes.insert(selected) }
        let titles = ["All servers"] + homes.sorted()
        guard titles != homeFilter.itemTitles else { return }
        homeFilter.removeAllItems(); homeFilter.addItems(withTitles: titles)
        homeFilter.selectItem(withTitle: selected)
    }
    func pauseFollowing() {
        guard following else { return }
        following = false; logDocument.followsTail = false; updateSummary()
    }
    @objc func toggleFollow() {
        following.toggle()
        logDocument.followsTail = following
        if following { logDocument.scrollToBottom(); refresh() }
        updateSummary()
    }
    @objc private func filtersChanged() { render() }
    func controlTextDidChange(_ notification: Notification) { render() }
    @objc private func copyLogs() {
        guard !logDocument.visibleText.isEmpty else { return }
        NSPasteboard.general.clearContents()
        if NSPasteboard.general.setString(logDocument.visibleText, forType: .string) {
            summary.stringValue = "Visible logs copied"
        } else { summary.stringValue = "Could not copy logs. Select text and try Copy again." }
    }
    func render() {
        let level = levelFilter.titleOfSelectedItem == "All levels" ? nil : levelFilter.titleOfSelectedItem
        let home = homeFilter.titleOfSelectedItem == "All servers" ? nil : homeFilter.titleOfSelectedItem
        let entries = history.matching(search: search.stringValue, level: level, home: home)
        let empty = !history.loaded && failure == nil ? "Loading worker logs…" : failure != nil && history.entries.isEmpty ? "Worker logs are unavailable. Use Refresh to try again." : history.entries.isEmpty ? "No worker log entries yet." : "No matching worker log entries."
        logDocument.render(entries, following: following, emptyText: empty)
        copyButton.isEnabled = !entries.isEmpty
        updateSummary()
    }
    private func updateSummary() {
        refreshButton.isEnabled = !loading
        followButton.title = following ? "Pause follow" : "Resume follow"
        followButton.setAccessibilityLabel(following ? "Pause following worker logs" : "Resume following worker logs")
        let count = logDocument.rendered.count == history.entries.count ? "\(history.entries.count) entries" : "\(logDocument.rendered.count) of \(history.entries.count) entries"
        let status = "\(count) · \(following ? "Following" : "Follow paused")"
        let notice = failure.map { _ in "Worker unavailable; retained logs" } ?? history.boundary
        summary.stringValue = status + (notice.map { " · " + $0 } ?? (loading ? " · Updating…" : "")) + (history.discarded > 0 ? " · Oldest entries discarded" : "")
        summary.toolTip = [summary.stringValue, failure, "Local diagnostics. Credentials are redacted. Nothing is uploaded."].compactMap { $0 }.joined(separator: "\n")
    }
}
