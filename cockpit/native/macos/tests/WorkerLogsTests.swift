import AppKit

@main
enum WorkerLogsTests {
    static func entry(_ number: Int, message: String? = nil, id: String? = nil) -> WorkerLogEntry {
        WorkerLogEntry(id: id ?? "file-a:\(number)", time: "2026-11-08T18:00:00Z", level: number % 7 == 0 ? "WARN" : "INFO",
                       home: number % 3 == 0 ? "production" : "lab", message: message ?? "Entry \(number): worker connection remains healthy. " + String(repeating: "Diagnostic detail. ", count: 5))
    }
    static func page(_ entries: [WorkerLogEntry], cursor: String = "next", reset: String? = nil, more: Bool = false) -> WorkerLogPage {
        WorkerLogPage(entries: entries, cursor: cursor, hasMore: more, reset: reset)
    }
    static func close(_ lhs: CGFloat, _ rhs: CGFloat, _ message: String) {
        precondition(abs(lhs - rhs) < 2, "\(message): \(lhs) differs from \(rhs)")
    }

    static func main() {
        _ = NSApplication.shared
        var history = WorkerLogHistory()
        let repeated = [entry(1, message: "Same message"), entry(2, message: "Same message")]
        history.accept(page(repeated, cursor: "offset-2"))
        history.accept(page(repeated, cursor: "offset-2"))
        precondition(history.entries == repeated, "Snapshot replays dedupe IDs, not distinct repeated messages")
        history.accept(page([], cursor: "offset-2"))
        precondition(history.entries == repeated && history.cursor == "offset-2", "An empty poll preserves history")
        history.accept(page([entry(3, id: "file-b:0")], cursor: "file-b:1", reset: "rotated"))
        precondition(history.entries.count == 3 && history.boundary?.contains("rotated") == true, "Rotation retains old records with a visible boundary")
        history.accept(page([], cursor: "file-b:1", reset: "missing"))
        precondition(history.entries.count == 3, "A missing file cannot erase retained records")
        history.accept(page([entry(4)], cursor: "offset-4"))
        precondition(history.boundary == nil)
        history.accept(page([], reset: "expired"))
        precondition(history.boundary?.contains("unavailable") == true)
        history.maximumEntries = 2
        history.accept(page([entry(5)]))
        precondition(history.entries.map(\.id) == [entry(4).id, entry(5).id] && history.discarded == 3)
        history.maximumCharacters = 150
        history.accept(page([entry(6, message: String(repeating: "long ", count: 100))]))
        precondition(history.entries.isEmpty, "A character cap bounds unusually large records independently of row count")
        history = WorkerLogHistory()
        history.accept(page((0..<300).map { entry($0) }))
        precondition(history.matching(search: "ENTRY 21:", level: "WARN", home: "production").map(\.id) == [entry(21).id])
        precondition(history.matching(search: "not found", level: nil, home: nil).isEmpty)

        let document = WorkerLogDocument()
        let host = NSView(frame: NSRect(x: 0, y: 0, width: 920, height: 460))
        document.scrollView.frame = host.bounds
        host.addSubview(document.scrollView)
        let initial = (0..<300).map { entry($0) }
        document.render(initial, following: true, emptyText: "No entries")
        precondition(document.isAtBottom)
        precondition(document.textView.frame.width > 880, "The log uses the full viewport width")
        let layout = document.textView.layoutManager!, container = document.textView.textContainer!
        close(document.textView.frame.height, ceil(layout.usedRect(for: container).height) + 20, "Document height follows laid out text, not a stale viewport estimate")
        precondition(document.textView.frame.height > host.frame.height * 10)

        // Scrolling through the actual layout always intersects real glyphs;
        // an oversized or stale document frame would produce blank regions.
        for fraction in [CGFloat(0), 0.25, 0.6, 1] {
            let y = (document.textView.frame.height - document.scrollView.contentSize.height) * fraction
            document.scrollView.contentView.scroll(to: NSPoint(x: 0, y: y))
            let bounds = document.scrollView.contentView.bounds.offsetBy(dx: -document.textView.textContainerOrigin.x, dy: -document.textView.textContainerOrigin.y)
            precondition(layout.glyphRange(forBoundingRect: bounds, in: container).length > 100, "Scrollback must contain laid out log content")
        }

        let selected = (document.textView.string as NSString).range(of: "Entry 150:")
        document.textView.setSelectedRange(selected)
        document.textView.scrollRangeToVisible(selected)
        let origin = document.scrollView.contentView.bounds.origin
        var mutations = 0
        let observer = NotificationCenter.default.addObserver(forName: NSTextStorage.didProcessEditingNotification,
            object: document.textView.textStorage, queue: nil) { _ in mutations += 1 }
        document.render(initial, following: false, emptyText: "No entries")
        precondition(mutations == 0, "An unchanged refresh must never replace text storage")
        precondition(document.textView.selectedRange() == selected)
        close(document.scrollView.contentView.bounds.minY, origin.y, "An unchanged refresh preserves scrollback")
        document.render(initial + [entry(300)], following: false, emptyText: "No entries")
        precondition(document.textView.selectedRange() == selected, "Appending keeps selected text")
        close(document.scrollView.contentView.bounds.minY, origin.y, "Appending preserves the reading position")
        document.render(Array(initial[100...]) + [entry(300)], following: false, emptyText: "No entries")
        let retainedSelection = document.textView.selectedRange()
        precondition((document.textView.string as NSString).substring(with: retainedSelection) == "Entry 150:", "Dropping old records remaps selection by stable record identity")
        let selectedGlyphs = layout.glyphRange(forCharacterRange: retainedSelection, actualCharacterRange: nil)
        let selectedRect = layout.boundingRect(forGlyphRange: selectedGlyphs, in: container).offsetBy(dx: 0, dy: document.textView.textContainerOrigin.y)
        precondition(document.scrollView.contentView.bounds.intersects(selectedRect), "Trimming earlier history keeps the selected record in view")
        var unwantedPauses = 0
        document.userScrolledAway = { unwantedPauses += 1 }
        document.scrollView.setFrameSize(NSSize(width: 650, height: 300))
        precondition(document.textView.selectedRange() == retainedSelection, "Resizing preserves text selection")
        let resizedRect = layout.boundingRect(forGlyphRange: selectedGlyphs, in: container).offsetBy(dx: 0, dy: document.textView.textContainerOrigin.y)
        precondition(document.scrollView.contentView.bounds.intersects(resizedRect), "Reflow preserves the visible record")
        close(document.textView.frame.height, ceil(layout.usedRect(for: container).height) + 20, "Narrow viewport has fully laid out content")
        document.render(Array(initial[100...]).filter { $0.home == "production" }, following: false, emptyText: "No entries")
        precondition((document.textView.string as NSString).substring(with: document.textView.selectedRange()) == "Entry 150:", "Filtering preserves a surviving selection by identity")
        document.render(Array(initial[100...]) + [entry(300)], following: false, emptyText: "No entries")
        document.scrollToBottom()
        precondition(document.isAtBottom && document.textView.selectedRange() == retainedSelection, "Resume scrolls without replacing selection")
        document.followsTail = true
        document.scrollView.setFrameSize(NSSize(width: 1180, height: 760))
        precondition(document.isAtBottom, "Resizing while following stays at the actual end")
        precondition(unwantedPauses == 0, "Programmatic reflow and rendering cannot pause following")
        NotificationCenter.default.removeObserver(observer)

        let controller = WorkerLogsWindow()
        controller.window?.setFrameOrigin(NSPoint(x: -10000, y: -10000))
        var calls = 0
        var completion: ((WorkerLogPage?, String?) -> Void)?
        controller.read = { _, done in calls += 1; completion = done }
        controller.refresh()
        precondition(calls == 0, "Hidden logs do not poll")
        controller.window?.orderFront(nil)
        controller.refresh(); controller.refresh()
        precondition(calls == 1 && controller.loading, "Refresh requests cannot overlap")
        completion?(page(initial, cursor: "first"), nil)
        controller.logDocument.textView.setSelectedRange((controller.logDocument.textView.string as NSString).range(of: "Entry 150:"))
        precondition(!controller.following, "Selecting text pauses follow")
        controller.refresh()
        precondition(calls == 2, "Paused follow still receives new logs")
        let kept = controller.logDocument.textView.string
        let keptSelection = controller.logDocument.textView.selectedRange()
        completion?(nil, "Worker is temporarily unavailable.")
        precondition(controller.logDocument.textView.string == kept && controller.logDocument.textView.selectedRange() == keptSelection,
                     "A transient error preserves visible content and selection")
        precondition(controller.history.cursor == "first" && controller.failure != nil, "A failed read does not advance the cursor")
        controller.refresh(); completion?(page([], cursor: "first"), nil)
        precondition(controller.logDocument.textView.string == kept && controller.failure == nil)
        controller.search.stringValue = "no matching diagnostics"
        controller.render()
        precondition(controller.logDocument.rendered.isEmpty && !controller.copyButton.isEnabled)
        precondition(controller.logDocument.textView.string == "No matching worker log entries.")
        controller.search.stringValue = ""; controller.render()
        precondition(controller.logDocument.textView.string == kept && controller.copyButton.isEnabled, "Clearing search restores all retained records")
        controller.toggleFollow()
        precondition(controller.following && controller.logDocument.isAtBottom)
        completion?(page([entry(300)], cursor: "second"), nil)
        precondition(controller.logDocument.isAtBottom)
        controller.logDocument.scrollView.contentView.scroll(to: NSPoint(x: 0, y: 10))
        precondition(!controller.following, "Scrolling away from the bottom pauses follow")
        controller.refresh(); completion?(page([entry(301)], cursor: "third", reset: "rotated"), nil)
        precondition(controller.summary.stringValue.contains("Follow paused") && controller.summary.stringValue.contains("302 entries") && controller.summary.stringValue.contains("rotated"), "Gap notices preserve count and follow state")
        controller.refresh(); completion?(page([], cursor: "third", more: true), nil)
        let stalledCalls = calls
        RunLoop.main.run(until: Date(timeIntervalSinceNow: 0.03))
        precondition(calls == stalledCalls, "A nonadvancing cursor cannot create an immediate polling loop")
        controller.refresh(); completion?(page([entry(302)], cursor: "fourth", more: true), nil)
        let backlogCalls = calls
        RunLoop.main.run(until: Date(timeIntervalSinceNow: 0.03))
        precondition(calls == backlogCalls + 1 && controller.loading, "An advancing backlog requests the next page without waiting for the poll timer")
        completion?(page([], cursor: "fourth"), nil)
        controller.window?.orderOut(nil)
        let closedCalls = calls
        controller.refresh()
        precondition(calls == closedCalls)
        print("Worker logs verified: bounded incremental history, stable selection/scroll, complete layout, visible-only polling, follow and failure recovery.")
    }
}
