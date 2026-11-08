import AppKit

/// The update command returns public release and local service facts only.
/// Enrollment credentials never cross this UI boundary.
struct CockpitUpdateReply: Decodable {
    let ok: Bool
    var current_version: String?
    var latest_version: String?
    var variant: String?
    var available: Bool?
    var can_install: Bool?
    var restart_required: Bool?
    var release_url: String?
    var updated: Bool?
    var worker_reconnected: Bool?
    var connected: Int?
    var total: Int?
    var phase: String?
    var message: String?
    var error: String?
    var operation_id: String?
    var updated_at: String?

    func processError(exitStatus: Int32) -> String? {
        if let error, !error.isEmpty { return error }
        if exitStatus != 0 { return "The update command did not finish successfully. Check the worker and try again." }
        if !ok { return "The update request failed. Try again." }
        return nil
    }

    var timestamp: Date? {
        guard let updated_at else { return nil }
        let parser = ISO8601DateFormatter()
        parser.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = parser.date(from: updated_at) { return date }
        parser.formatOptions = [.withInternetDateTime]
        return parser.date(from: updated_at)
    }
}

enum CockpitUpdateAction { case check, install }

/// Keep the last verified release visible through a transient check failure.
/// Download completion and worker reconnection remain distinct facts.
struct CockpitUpdateState {
    private(set) var release: CockpitUpdateReply?
    private(set) var checking = false
    private(set) var installing = false
    private(set) var phase = "idle"
    private(set) var failure: String?
    private(set) var outcome: String?
    private var outcomeDetail: String?
    private var updatedVersion: String?

    var hasUpdate: Bool { release?.restart_required == true || (release?.available == true && release?.updated != true) }
    var action: CockpitUpdateAction? {
        if checking || installing { return nil }
        return hasUpdate && release?.can_install == true ? .install : .check
    }
    var actionTitle: String {
        if installing { return "Updating Cockpit…" }
        if checking { return "Checking for Updates…" }
        if action == .install && release?.restart_required == true { return "Restart to Finish Update" }
        if action == .install { return "Update to \(release?.latest_version ?? "new version") and Restart" }
        return failure == nil ? "Check for Updates…" : "Retry Update Check"
    }
    var title: String {
        if checking { return "Checking for updates…" }
        if installing {
            switch phase {
            case "downloading": return "Downloading update…"
            case "verifying": return "Verifying update…"
            case "installing": return "Installing update…"
            case "restarting": return "Restarting the worker…"
            case "reconnecting": return "Reconnecting to your servers…"
            default: return "Preparing update…"
            }
        }
        if failure != nil { return "Update needs attention" }
        if let outcome { return outcome }
        if release?.restart_required == true { return "Restart to finish updating Cockpit" }
        if hasUpdate { return "Version \(release?.latest_version ?? "unknown") is available" }
        if release != nil { return "Cockpit is up to date" }
        return "Updates have not been checked"
    }
    var detail: String {
        if let failure { return failure }
        if let outcomeDetail { return outcomeDetail }
        if !installing && release?.restart_required == true {
            return release?.message ?? "The new version is installed. Restart the worker to finish reconnecting."
        }
        if installing || (hasUpdate && release?.can_install == true) {
            return "Cockpit will restart. Your servers and connection choices are kept. macOS may ask for permissions again."
        }
        if hasUpdate, let message = release?.message, !message.isEmpty { return message }
        return ""
    }
    mutating func beginCheck() -> Bool {
        guard !checking, !installing else { return false }
        checking = true
        return true
    }
    mutating func checked(_ reply: CockpitUpdateReply?, error: String?) {
        checking = false
        guard let reply, reply.ok, error == nil else {
            failure = error ?? reply?.error ?? "Could not check for updates. Try again."
            return
        }
        release = reply
        failure = nil
        if hasUpdate { outcome = nil; outcomeDetail = nil; updatedVersion = nil }
    }
    mutating func beginInstall() -> String? {
        guard action == .install, let version = release?.latest_version, !version.isEmpty else { return nil }
        installing = true; phase = "starting"; failure = nil; outcome = nil; outcomeDetail = nil
        return version
    }
    mutating func progress(_ reply: CockpitUpdateReply) {
        guard installing, let phase = reply.phase,
              ["starting", "downloading", "verifying", "installing", "restarting", "reconnecting"].contains(phase) else { return }
        self.phase = phase
    }
    mutating func finished(_ reply: CockpitUpdateReply?, error: String?) {
        installing = false; phase = reply?.phase ?? "error"
        if let reply, reply.updated == true {
            release = reply
            updatedVersion = reply.current_version
            if reply.total == 0 { outcome = "Cockpit updated" }
            else { outcome = reply.worker_reconnected == true ? "Cockpit updated and connected" : "Cockpit updated; connections pending" }
            failure = error ?? (reply.ok ? nil : (reply.error ?? "The update did not finish successfully. Check the worker and try again."))
            // An unavailable cluster is not a failed download or an invitation
            // to install the same release repeatedly.
            if reply.worker_reconnected != true, let message = reply.message, !message.isEmpty { outcomeDetail = message }
        } else {
            failure = error ?? reply?.error ?? "The update did not finish. Try again."
        }
    }
    mutating func observeConnections(workerVersion: String, enabled: Int, connected: Int) {
        guard let updatedVersion,
              workerVersion.split(separator: "+").first == updatedVersion.split(separator: "+").first,
              enabled > 0, connected == enabled else { return }
        outcome = "Cockpit updated and connected"; outcomeDetail = nil
    }
}

enum CockpitMenuIcon {
    static func image(updateAvailable: Bool, resourceURL: URL? = nil) -> NSImage? {
        guard updateAvailable else { return MarkParser.image(resourceURL: resourceURL) }
        guard let mark = MarkParser.image(resourceURL: resourceURL) else { return nil }
        // The arrow adds meaning without relying on color, and remains a
        // template so macOS supplies contrast on either menu bar appearance.
        let image = NSImage(size: NSSize(width: 28, height: 20), flipped: false) { _ in
            mark.draw(in: NSRect(x: 0, y: 0, width: 20, height: 20))
            NSColor.black.setStroke()
            let arrow = NSBezierPath()
            arrow.lineWidth = 1.5; arrow.lineCapStyle = .round; arrow.lineJoinStyle = .round
            arrow.move(to: NSPoint(x: 24, y: 5)); arrow.line(to: NSPoint(x: 24, y: 15))
            arrow.move(to: NSPoint(x: 21, y: 12)); arrow.line(to: NSPoint(x: 24, y: 15)); arrow.line(to: NSPoint(x: 27, y: 12))
            arrow.stroke()
            return true
        }
        image.isTemplate = true
        return image
    }
}

final class CockpitAboutWindow: NSWindowController {
    var action: (() -> Void)?
    private let version = NSTextField(wrappingLabelWithString: "")
    private let worker = NSTextField(wrappingLabelWithString: "")
    private let status = NSTextField(wrappingLabelWithString: "")
    private let detail = NSTextField(wrappingLabelWithString: "")
    private let button = NSButton(title: "Check for Updates…", target: nil, action: nil)

    init(logo: NSImage? = nil) {
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 430, height: 320),
                              styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.title = "About MemQL Cockpit"; window.isReleasedWhenClosed = false
        super.init(window: window)
        let content = window.contentView!
        let mark = NSImageView()
        mark.image = logo ?? MarkParser.image(size: 48, template: false)
        let name = NSTextField(labelWithString: "MemQL Cockpit")
        name.font = NSFont.systemFont(ofSize: 20, weight: .semibold)
        version.font = NSFont.systemFont(ofSize: 12); version.textColor = .secondaryLabelColor; version.isSelectable = true
        worker.font = NSFont.systemFont(ofSize: 12); worker.textColor = .secondaryLabelColor; worker.isSelectable = true
        status.font = NSFont.systemFont(ofSize: 13, weight: .medium)
        detail.font = NSFont.systemFont(ofSize: 12); detail.textColor = .secondaryLabelColor; detail.isSelectable = true
        button.bezelStyle = .rounded; button.target = self; button.action = #selector(performAction)
        button.keyEquivalent = "\r"
        for view in [mark, name, version, worker, status, detail, button] {
            view.translatesAutoresizingMaskIntoConstraints = false; content.addSubview(view)
        }
        NSLayoutConstraint.activate([
            mark.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 24),
            mark.topAnchor.constraint(equalTo: content.topAnchor, constant: 24),
            mark.widthAnchor.constraint(equalToConstant: 48), mark.heightAnchor.constraint(equalToConstant: 48),
            name.leadingAnchor.constraint(equalTo: mark.trailingAnchor, constant: 16),
            name.topAnchor.constraint(equalTo: mark.topAnchor, constant: 2),
            version.leadingAnchor.constraint(equalTo: name.leadingAnchor),
            version.topAnchor.constraint(equalTo: name.bottomAnchor, constant: 6),
            version.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -24),
            worker.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 24),
            worker.topAnchor.constraint(equalTo: version.bottomAnchor, constant: 24),
            worker.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -24),
            status.leadingAnchor.constraint(equalTo: worker.leadingAnchor),
            status.trailingAnchor.constraint(equalTo: worker.trailingAnchor),
            status.topAnchor.constraint(equalTo: worker.bottomAnchor, constant: 24),
            detail.leadingAnchor.constraint(equalTo: worker.leadingAnchor),
            detail.trailingAnchor.constraint(equalTo: worker.trailingAnchor),
            detail.topAnchor.constraint(equalTo: status.bottomAnchor, constant: 8),
            detail.bottomAnchor.constraint(lessThanOrEqualTo: button.topAnchor, constant: -20),
            button.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -24),
            button.bottomAnchor.constraint(equalTo: content.bottomAnchor, constant: -20)
        ])
        window.center()
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }
    @objc private func performAction() { action?() }

    func update(menuVersion: String, workerVersion: String?, connectionSummary: String, state: CockpitUpdateState) {
        version.stringValue = "Version \(menuVersion)"
        worker.stringValue = workerVersion.map { "Worker \($0)\n\(connectionSummary)" } ?? connectionSummary
        status.stringValue = state.title
        detail.stringValue = state.detail
        button.title = state.actionTitle
        button.isEnabled = state.action != nil
        // Longer network errors and build stamps should grow the window,
        // never truncate the remedy or overlap the action.
        let width: CGFloat = 382
        func height(_ field: NSTextField, width: CGFloat) -> CGFloat {
            field.cell?.cellSize(forBounds: NSRect(x: 0, y: 0, width: width, height: 1000)).height ?? 0
        }
        let wanted = max(320, 160 + height(version, width: 318) + height(worker, width: width) + height(status, width: width) + height(detail, width: width))
        window?.setContentSize(NSSize(width: 430, height: wanted))
    }
}
