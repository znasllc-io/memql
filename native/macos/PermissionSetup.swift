import AppKit

struct PermissionReport {
    let pid: Int
    let version: String
    let executable: String?
    let accessibility: String
    let screenRecording: String
    let requestsSupported: Bool
    let requestPending: Bool
    let checkedAt: Date?

    func fresh(at now: Date = Date()) -> Bool {
        guard let checkedAt else { return false }
        return now.timeIntervalSince(checkedAt) >= -5 && now.timeIntervalSince(checkedAt) <= 15
    }
    func granted(at now: Date = Date()) -> Bool {
        fresh(at: now) && accessibility == "Granted" && screenRecording == "Granted"
    }
}

enum PermissionBridge {
    static func accepts(_ url: URL) -> Bool {
        url.scheme == "memql-cockpit" && url.host == "permissions" &&
        (url.path.isEmpty || url.path == "/") && url.user == nil && url.password == nil &&
        url.port == nil && url.query == nil && url.fragment == nil
    }

    static func serviceMatches(_ output: String, pid: Int, executable: String) -> Bool {
        let lines = output.components(separatedBy: .newlines).map { $0.trimmingCharacters(in: .whitespaces) }
        guard lines.contains("pid = \(pid)") else { return false }
        if lines.contains("program = \(executable)") { return true }
        // MemQL.app registers the worker itself (memql-cockpit#545), and
        // launchd then names the program inside the bundle that registered
        // it rather than by path: MemQL.app's own main executable.
        return executable.hasSuffix(".app/Contents/MacOS/MemQL")
            && lines.contains("parent bundle identifier = com.znasllc.memql-worker")
            && lines.contains { $0.hasPrefix("program identifier = Contents/MacOS/MemQL ") || $0 == "program identifier = Contents/MacOS/MemQL" }
    }
}

struct PermissionRestartProgress {
    private(set) var pid: Int?
    private var deadline: Date?
    mutating func begin(pid: Int, now: Date = Date()) { self.pid = pid; deadline = now.addingTimeInterval(30) }
    mutating func cancel() { pid = nil; deadline = nil }
    mutating func observe(_ report: PermissionReport?, now: Date = Date()) -> String? {
        guard let oldPID = pid else { return nil }
        if let report, report.fresh(at: now), report.pid != oldPID {
            cancel()
            if report.granted(at: now) { return "Restart complete. Both permissions are confirmed." }
            return "Restart complete. Review the current permissions above."
        }
        if let deadline, now > deadline {
            cancel()
            return "A new worker process has not reported back. Open Cockpit Logs to check the service."
        }
        return nil
    }
    func ready(_ report: PermissionReport?, now: Date = Date()) -> Bool {
        pid == nil && report?.granted(at: now) == true
    }
}

// The window only reflects observations. A successful request or restart is
// never promoted to a grant; the next fresh worker report supplies that fact.
final class PermissionSetupWindow: NSWindowController {
    var request: ((String, Int) -> Void)?
    var restart: ((Int) -> Void)?
    var reveal: (() -> Void)?
    private var report: PermissionReport?
    private var progress = PermissionRestartProgress()
    private var message = ""
    private let summary = NSTextField(wrappingLabelWithString: "Connecting to the background worker…")
    private let identity = NSTextField(wrappingLabelWithString: "")
    private let feedback = NSTextField(wrappingLabelWithString: "")
    private let axStatus = NSTextField(labelWithString: "Unknown")
    private let screenStatus = NSTextField(labelWithString: "Unknown")
    private var detailsButton: NSButton!
    private var detailsVisible = false
    private var axRequest: NSButton!
    private var screenRequest: NSButton!
    private var restartButton: NSButton!

    init(logoImage: NSImage? = MarkParser.image(size: 48, template: false)) {
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 700, height: 520),
                              styleMask: [.titled, .closable, .miniaturizable], backing: .buffered, defer: false)
        window.title = "MemQL — Permissions"
        window.isReleasedWhenClosed = false
        super.init(window: window)
        let logo = NSImageView(image: logoImage ?? NSImage())
        logo.setAccessibilityLabel("MemQL logo")
        logo.widthAnchor.constraint(equalToConstant: 48).isActive = true
        logo.heightAnchor.constraint(equalToConstant: 48).isActive = true
        let heading = NSTextField(labelWithString: "MemQL")
        heading.font = .systemFont(ofSize: 24, weight: .semibold)
        let subtitle = NSTextField(labelWithString: "Permissions for this Mac")
        subtitle.font = .systemFont(ofSize: 13); subtitle.textColor = .secondaryLabelColor
        let brand = NSStackView(views: [heading, subtitle]); brand.orientation = .vertical; brand.alignment = .leading; brand.spacing = 3
        let header = NSStackView(views: [logo, brand]); header.spacing = 16
        summary.font = .systemFont(ofSize: 14)
        identity.font = .systemFont(ofSize: 11)
        identity.textColor = .secondaryLabelColor
        identity.isSelectable = true
        feedback.font = .systemFont(ofSize: 12)
        axRequest = NSButton(title: "Request access", target: self, action: #selector(requestAX))
        axRequest.setAccessibilityLabel("Request Accessibility")
        screenRequest = NSButton(title: "Request access", target: self, action: #selector(requestScreen))
        screenRequest.setAccessibilityLabel("Request Screen Recording")
        let ax = permissionRow("Accessibility", detail: "Allows mouse and keyboard control.", status: axStatus,
                               request: axRequest, settings: #selector(openAX))
        let screen = permissionRow("Screen Recording", detail: "Allows the worker to see your screen.", status: screenStatus,
                                   request: screenRequest, settings: #selector(openScreen))
        let permissions = NSBox(); permissions.boxType = .custom; permissions.borderWidth = 0.5
        permissions.titlePosition = .noTitle
        permissions.borderColor = .separatorColor; permissions.fillColor = .controlBackgroundColor; permissions.cornerRadius = 10
        permissions.contentViewMargins = NSSize(width: 16, height: 14)
        let divider = NSBox(); divider.boxType = .separator
        let rows = NSStackView(views: [ax, divider, screen]); rows.orientation = .vertical; rows.alignment = .leading; rows.spacing = 15
        rows.translatesAutoresizingMaskIntoConstraints = false
        permissions.contentView!.addSubview(rows)
        NSLayoutConstraint.activate([
            rows.leadingAnchor.constraint(equalTo: permissions.contentView!.leadingAnchor), rows.trailingAnchor.constraint(equalTo: permissions.contentView!.trailingAnchor),
            rows.topAnchor.constraint(equalTo: permissions.contentView!.topAnchor), rows.bottomAnchor.constraint(equalTo: permissions.contentView!.bottomAnchor)
        ])
        for row in [ax, divider, screen] { row.widthAnchor.constraint(equalTo: rows.widthAnchor).isActive = true }
        let reveal = NSButton(title: "Show MemQL in Finder", target: self, action: #selector(showWorker))
        restartButton = NSButton(title: "Restart Worker…", target: self, action: #selector(confirmRestart))
        let recovery = NSStackView(views: [reveal, restartButton]); recovery.spacing = 12
        let tip = NSTextField(wrappingLabelWithString: "If access stays denied while Settings shows MemQL enabled, remove its old entry and add this app again. After approval, restart once to verify.")
        tip.font = .systemFont(ofSize: 12); tip.textColor = .secondaryLabelColor
        detailsButton = NSButton(title: "Show technical details", target: self, action: #selector(toggleDetails))
        detailsButton.bezelStyle = .rounded; detailsButton.controlSize = .small; detailsButton.font = .systemFont(ofSize: 11)
        identity.isHidden = true
        let stack = NSStackView(views: [header, summary, permissions, recovery, tip, feedback, detailsButton, identity])
        stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = 18
        stack.translatesAutoresizingMaskIntoConstraints = false
        window.contentView!.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: window.contentView!.leadingAnchor, constant: 24),
            stack.trailingAnchor.constraint(equalTo: window.contentView!.trailingAnchor, constant: -24),
            stack.topAnchor.constraint(equalTo: window.contentView!.topAnchor, constant: 24),
            stack.bottomAnchor.constraint(lessThanOrEqualTo: window.contentView!.bottomAnchor, constant: -24)
        ])
        for view in [summary, permissions, tip, feedback, identity] {
            view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        }
        window.center()
        update(nil)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    private func permissionRow(_ title: String, detail: String, status: NSTextField, request: NSButton, settings: Selector) -> NSStackView {
        let name = NSTextField(labelWithString: title); name.font = .systemFont(ofSize: 14, weight: .semibold)
        let description = NSTextField(labelWithString: detail); description.font = .systemFont(ofSize: 12)
        description.textColor = .secondaryLabelColor
        let text = NSStackView(views: [name, description, status]); text.orientation = .vertical; text.alignment = .leading; text.spacing = 3
        status.font = .systemFont(ofSize: 12, weight: .medium)
        let settings = NSButton(title: "Settings…", target: self, action: settings)
        settings.setAccessibilityLabel("Open \(title) Settings")
        request.bezelStyle = .rounded; settings.bezelStyle = .rounded
        request.controlSize = .regular; settings.controlSize = .regular
        let spacer = NSView()
        let row = NSStackView(views: [text, spacer, request, settings]); row.spacing = 8
        return row
    }

    func update(_ report: PermissionReport?) {
        self.report = report
        let fresh = report?.fresh() == true
        if let result = progress.observe(report) { message = result }
        let restarting = progress.pid != nil
        let supported = fresh && report?.requestsSupported == true
        let pending = report?.requestPending == true
        axStatus.stringValue = fresh ? report!.accessibility : "Unknown — waiting for worker"
        screenStatus.stringValue = fresh ? report!.screenRecording : "Unknown — waiting for worker"
        axStatus.textColor = fresh && report?.accessibility == "Granted" ? .systemGreen : .secondaryLabelColor
        screenStatus.textColor = fresh && report?.screenRecording == "Granted" ? .systemGreen : .secondaryLabelColor
        axRequest.isEnabled = supported && !pending && !restarting && report?.accessibility != "Granted"
        screenRequest.isEnabled = supported && !pending && !restarting && report?.screenRecording != "Granted"
        restartButton.isEnabled = fresh && report?.executable != nil && !pending && !restarting
        if restarting {
            summary.stringValue = "Waiting for the restarted worker to report its permissions…"
        } else if progress.ready(report) {
            summary.stringValue = "Ready. MemQL has confirmed access to your screen and controls."
            message = ""
        } else if !fresh {
            summary.stringValue = "Waiting for a fresh report from the background worker. Access is not yet verified."
        } else if !supported {
            summary.stringValue = "Guided requests need an updated macOS computer-use worker. Install or update Cockpit, then reopen this window."
        } else {
            summary.stringValue = "Let MemQL see your screen and use your mouse and keyboard. Approve each request in macOS; we’ll verify access here."
        }
        feedback.stringValue = pending ? "Waiting for the macOS permission dialog…" : message
        identity.stringValue = report.map { "Worker \($0.version) · PID \($0.pid)\n\($0.executable ?? "Executable path unavailable from this worker")" } ?? "Worker unavailable"
    }
    func actionFinished(_ error: String?) {
        message = error ?? "Request sent. Only a fresh worker report can confirm access."
        update(report)
    }
    func restartFinished(_ error: String?) {
        if let error { progress.cancel(); message = error }
        update(report)
    }
    @objc private func requestAX() { requestPermission("accessibility") }
    @objc private func requestScreen() { requestPermission("screen_recording") }
    private func requestPermission(_ permission: String) {
        guard let report, report.fresh(), report.requestsSupported else { return }
        axRequest.isEnabled = false; screenRequest.isEnabled = false
        request?(permission, report.pid)
    }
    @objc private func openAX() { NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility")!) }
    @objc private func openScreen() { NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture")!) }
    @objc private func showWorker() { reveal?() }
    @objc private func toggleDetails() {
        detailsVisible.toggle(); identity.isHidden = !detailsVisible
        detailsButton.title = detailsVisible ? "Hide technical details" : "Show technical details"
        window?.setContentSize(NSSize(width: 700, height: detailsVisible ? 600 : 520))
    }
    @objc private func confirmRestart() {
        guard let report, report.fresh() else { return }
        let alert = NSAlert()
        alert.messageText = "Restart the background worker?"
        alert.informativeText = "This interrupts running work and briefly disconnects every server on this Mac. Your enrollments and paused connections are preserved. macOS permissions will be checked again by the new worker process."
        alert.addButton(withTitle: "Cancel"); alert.addButton(withTitle: "Restart Worker")
        guard alert.runModal() == .alertSecondButtonReturn else { return }
        progress.begin(pid: report.pid)
        update(self.report)
        restart?(report.pid)
    }
}
