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
    /// Setup on this Mac is waiting for the person here (memql-cockpit#548):
    /// the installer's permissions stage is running and nobody has answered.
    var setupWaiting = false

    func fresh(at now: Date = Date()) -> Bool {
        guard let checkedAt else { return false }
        return now.timeIntervalSince(checkedAt) >= -5 && now.timeIntervalSince(checkedAt) <= 15
    }
    func granted(at now: Date = Date()) -> Bool {
        fresh(at: now) && accessibility == "Granted" && screenRecording == "Granted"
    }
}

/// The installer's report as the worker's status carries it (memql-cockpit#548).
struct SetupStageStatus: Decodable {
    let name: String
    let state: String
}
struct SetupAnswerStatus: Decodable {
    let stage: String
    let choice: String
}
struct SetupStatus: Decodable {
    let state: String
    let stages: [SetupStageStatus]?
    let answers: [String: SetupAnswerStatus]?

    /// The installer is waiting for the person in the Permissions window.
    var waitingForPermissions: Bool {
        state == "running" && answers?["permissions"] == nil &&
        (stages ?? []).contains { $0.name == "permissions" && $0.state == "running" }
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
        return lines.contains("pid = \(pid)") && lines.contains("program = \(executable)")
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

// Switches always show fresh worker observations. Asking macOS is not a
// grant. Enabling uses the system prompt alone; disabling must be done in
// Settings. Explicit Settings links also cover a previously denied prompt.
// Never open Settings alongside a system prompt or dismiss Apple's dialogs.

enum PermissionKind: String, CaseIterable {
    case accessibility
    case screenRecording = "screen_recording"

    var title: String { self == .accessibility ? "Accessibility" : "Screen Recording" }
    var detail: String { self == .accessibility ? "Use the mouse and keyboard" : "See the screen" }
    var settingsURL: URL {
        let pane = self == .accessibility ? "Privacy_Accessibility" : "Privacy_ScreenCapture"
        return URL(string: "x-apple.systempreferences:com.apple.preference.security?\(pane)")!
    }
    func granted(in report: PermissionReport) -> Bool {
        (self == .accessibility ? report.accessibility : report.screenRecording) == "Granted"
    }
}

struct PermissionRow: Equatable {
    var on: Bool
    var enabled: Bool
    /// What to do in System Settings, "" for nothing.
    var note: String
    var pending = false
}

enum PermissionAct: Equatable { case done, restart }
enum PermissionRequestAction: Equatable { case prompt, settings }

/// The window's floor: the state in words, and at most one act.
struct PermissionFloor: Equatable {
    var state: String
    var busy: Bool
    var act: PermissionAct?
}

struct PermissionFlow {
    /// The position the person asked System Settings for, by permission,
    /// until the worker reports it.
    private(set) var asks: [PermissionKind: Bool] = [:]
    /// A permission request can require a restart. A fresh report
    /// from the replacement worker settles it even when still denied.
    private var requestPID: Int?

    var asking: Bool { !asks.isEmpty }

    // One effect per gesture, shared by both permission switches. A pending
    // OS request must be answered before another can be sent.
    static func action(on: Bool, report: PermissionReport?, restarting: Bool, now: Date = Date()) -> PermissionRequestAction? {
        guard let report, report.fresh(at: now), report.requestsSupported,
              !report.requestPending, !restarting else { return nil }
        return on ? .prompt : .settings
    }

    mutating func begin(_ kind: PermissionKind, on: Bool, pid: Int) {
        asks[kind] = on
        requestPID = pid
    }
    mutating func cancel(_ kind: PermissionKind) { asks[kind] = nil }
    /// Offscreen previews only: asks made under `pid`, without opening
    /// System Settings.
    mutating func preview(_ asks: [PermissionKind: Bool], pid: Int?) { self.asks = asks; requestPID = pid }

    /// Settles each ask on its requested result or a new worker's report.
    /// Closing Settings alone cannot establish whether the grant took effect.
    mutating func observe(_ report: PermissionReport?, now: Date = Date()) -> Bool {
        guard let report, report.fresh(at: now) else { return false }
        let before = asks.count
        asks = asks.filter { kind, on in
            kind.granted(in: report) != on && report.pid == requestPID
        }
        return asks.count != before
    }

    func row(_ kind: PermissionKind, _ report: PermissionReport?, restarting: Bool, now: Date = Date()) -> PermissionRow {
        guard let report, report.fresh(at: now) else {
            return PermissionRow(on: false, enabled: false, note: restarting ? "Applying after restart" : "", pending: restarting && asks[kind] != nil)
        }
        let granted = kind.granted(in: report)
        let enabled = report.requestsSupported && !restarting && !report.requestPending
        guard asks[kind] != nil else { return PermissionRow(on: granted, enabled: enabled, note: "") }
        return PermissionRow(on: granted, enabled: enabled,
                             note: restarting ? "Applying after restart" : "Permission setup pending", pending: true)
    }

    /// While setup waits on this window (memql-cockpit#548): the one choice
    /// that lets it go on without what is not allowed yet, or nil. Allowing
    /// everything needs no choice -- the installer sees the grant itself.
    func setupChoice(_ report: PermissionReport?, restarting: Bool, now: Date = Date()) -> String? {
        guard let report, report.fresh(at: now), report.setupWaiting, !restarting else { return nil }
        let missing = PermissionKind.allCases.filter { !$0.granted(in: report) }
        if missing.isEmpty { return nil }
        return missing.count == PermissionKind.allCases.count ? "Skip for now" : "Continue without \(missing[0].title)"
    }

    func floor(_ report: PermissionReport?, restarting: Bool, now: Date = Date()) -> PermissionFloor {
        if restarting { return PermissionFloor(state: "Restarting MemQL…", busy: true, act: nil) }
        guard let report, report.fresh(at: now) else { return PermissionFloor(state: "Waiting for the worker", busy: true, act: nil) }
        guard report.requestsSupported else { return PermissionFloor(state: "Update Cockpit to set these up", busy: false, act: nil) }
        if report.requestPending { return PermissionFloor(state: "Waiting for macOS", busy: true, act: nil) }
        if asking {
            return PermissionFloor(state: "After approval, restart MemQL", busy: false, act: .restart)
        }
        if report.granted(at: now) { return PermissionFloor(state: "Ready", busy: false, act: .done) }
        let missing = PermissionKind.allCases.filter { !$0.granted(in: report) }
        return PermissionFloor(state: missing.count > 1 ? "Turn on both to finish" : "Turn on \(missing[0].title) to finish",
                               busy: false, act: nil)
    }
}

// The window only reflects observations. A request or a restart is never
// promoted to a grant; the next fresh worker report supplies that fact.
final class PermissionSetupWindow: NSWindowController, NSWindowDelegate {
    var request: ((String, Int) -> Void)?
    var restart: ((Int) -> Void)?
    var reveal: (() -> Void)?
    /// The person chose to let setup go on without what is not allowed yet.
    var skip: (() -> Void)?
    /// Poll faster while a request is awaiting an observation.
    var asking: ((Bool) -> Void)?
    var closed: (() -> Void)?

    private var report: PermissionReport?
    private var progress = PermissionRestartProgress()
    private var flow = PermissionFlow()
    private var message = ""
    private var settingsObserver: NSObjectProtocol?
    private var requestingPermission: PermissionKind?
    /// Settings was opened explicitly from this window.
    private var inSettings = false {
        didSet {
            guard inSettings != oldValue else { return }
            asking?(inSettings)
        }
    }
    private var switches: [PermissionKind: MemQLSwitch] = [:]
    private var notes: [PermissionKind: NSTextField] = [:]
    private let state = NSTextField(labelWithString: "")
    private let spinner = NSProgressIndicator()
    private let feedback = NSTextField(wrappingLabelWithString: "")
    /// Shown only while setup waits on this window: why it matters now, and
    /// the way on without what is not allowed yet.
    private var setupRow: NSStackView!
    private var setupSkip: NSButton!
    private let identity = NSTextField(wrappingLabelWithString: "")
    private var act: NSButton!
    private var troubleshootingButton: NSButton!
    private var troubleshooting: NSStackView!
    private var stack: NSStackView!

    static let width: CGFloat = 400

    init(logoImage: NSImage? = MarkParser.image(size: 48, template: false)) {
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: Self.width, height: 320),
                              styleMask: [.titled, .closable, .miniaturizable, .fullSizeContentView], backing: .buffered, defer: false)
        window.title = "MemQL Permissions"
        window.titleVisibility = .hidden
        window.titlebarAppearsTransparent = true
        window.isMovableByWindowBackground = true
        window.isReleasedWhenClosed = false
        // Stay available when the person follows the native dialog to Settings.
        window.hidesOnDeactivate = false
        super.init(window: window)
        window.delegate = self

        let logo = NSImageView(image: logoImage ?? NSImage())
        logo.setAccessibilityLabel("MemQL")
        logo.widthAnchor.constraint(equalToConstant: 32).isActive = true
        logo.heightAnchor.constraint(equalToConstant: 32).isActive = true
        let title = label("Permissions", size: 17, weight: .semibold)
        let sentence = label("Let MemQL use this Mac for you.", size: 12, color: .secondaryLabelColor)
        let titles = NSStackView(views: [title, sentence]); titles.orientation = .vertical; titles.alignment = .leading; titles.spacing = 2
        let header = NSStackView(views: [logo, titles]); header.spacing = 12; header.alignment = .centerY

        let rows = NSStackView(views: [row(.accessibility), separator(), row(.screenRecording)])
        rows.orientation = .vertical; rows.alignment = .leading; rows.spacing = 14

        spinner.style = .spinning; spinner.controlSize = .small; spinner.isDisplayedWhenStopped = false
        state.font = .systemFont(ofSize: 12, weight: .medium)
        act = NSButton(title: "Done", target: self, action: #selector(primaryAct))
        act.bezelStyle = .rounded; act.keyEquivalent = "\r"
        let floor = NSStackView(views: [spinner, state, NSView(), act]); floor.spacing = 8; floor.alignment = .centerY
        feedback.font = .systemFont(ofSize: 11); feedback.textColor = .secondaryLabelColor
        let setupNote = label("Setup is waiting for you.", size: 11, color: .secondaryLabelColor)
        setupSkip = link("Skip for now", #selector(skipForNow))
        setupRow = NSStackView(views: [setupNote, NSView(), setupSkip]); setupRow.spacing = 8; setupRow.alignment = .centerY
        setupRow.isHidden = true

        troubleshootingButton = NSButton(title: "Troubleshooting", target: self, action: #selector(toggleTroubleshooting))
        troubleshootingButton.bezelStyle = .disclosure; troubleshootingButton.setButtonType(.pushOnPushOff); troubleshootingButton.title = ""
        let disclosureLabel = NSButton(title: "Troubleshooting", target: self, action: #selector(toggleTroubleshooting))
        disclosureLabel.isBordered = false; disclosureLabel.font = .systemFont(ofSize: 11); disclosureLabel.contentTintColor = .secondaryLabelColor
        let disclosure = NSStackView(views: [troubleshootingButton, disclosureLabel]); disclosure.spacing = 2
        let tip = NSTextField(wrappingLabelWithString: "Switches confirm access for the running MemQL worker. If Settings shows an older build enabled, remove that MemQL entry and add the current app shown in Finder. A local development build may need approval again after an update.")
        tip.font = .systemFont(ofSize: 11); tip.textColor = .secondaryLabelColor
        let finder = link("Show MemQL in Finder", #selector(showWorker))
        let restartLink = link("Restart MemQL…", #selector(confirmRestart))
        let links = NSStackView(views: [finder, restartLink]); links.spacing = 14
        let settingsLinks = NSStackView(views: [link("Accessibility Settings…", #selector(openAccessibilitySettings)),
                                               link("Screen Recording Settings…", #selector(openScreenRecordingSettings))])
        settingsLinks.spacing = 14
        identity.font = .monospacedSystemFont(ofSize: 10, weight: .regular); identity.textColor = .tertiaryLabelColor
        identity.isSelectable = true
        troubleshooting = NSStackView(views: [tip, settingsLinks, links, feedback, identity])
        troubleshooting.orientation = .vertical; troubleshooting.alignment = .leading; troubleshooting.spacing = 8
        troubleshooting.isHidden = true

        stack = NSStackView(views: [header, rows, floor, setupRow, disclosure, troubleshooting])
        stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = 18
        stack.setCustomSpacing(22, after: header)
        stack.setCustomSpacing(6, after: floor)
        stack.setCustomSpacing(6, after: setupRow)
        stack.setCustomSpacing(8, after: disclosure)
        stack.translatesAutoresizingMaskIntoConstraints = false
        window.contentView!.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: window.contentView!.leadingAnchor, constant: 24),
            stack.trailingAnchor.constraint(equalTo: window.contentView!.trailingAnchor, constant: -24),
            stack.topAnchor.constraint(equalTo: window.contentView!.topAnchor, constant: 36),
            stack.widthAnchor.constraint(equalToConstant: Self.width - 48),
        ])
        for view in [rows, floor, setupRow, feedback, troubleshooting, tip, identity] as [NSView] {
            view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        }
        for row in rows.arrangedSubviews { row.widthAnchor.constraint(equalTo: rows.widthAnchor).isActive = true }

        settingsObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didTerminateApplicationNotification, object: nil, queue: .main
        ) { [weak self] note in
            let app = note.userInfo?[NSWorkspace.applicationUserInfoKey] as? NSRunningApplication
            if app?.bundleIdentifier == "com.apple.systempreferences" { self?.systemSettingsClosed() }
        }
        update(nil)
        window.center()
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }
    deinit { if let settingsObserver { NSWorkspace.shared.notificationCenter.removeObserver(settingsObserver) } }

    private func label(_ text: String, size: CGFloat, weight: NSFont.Weight = .regular, color: NSColor = .labelColor) -> NSTextField {
        let field = NSTextField(labelWithString: text)
        field.font = .systemFont(ofSize: size, weight: weight); field.textColor = color
        return field
    }
    private func link(_ title: String, _ action: Selector) -> NSButton {
        let button = NSButton(title: title, target: self, action: action)
        button.isBordered = false; button.font = .systemFont(ofSize: 11); button.contentTintColor = MemQLColor.accent
        return button
    }
    private func separator() -> NSBox { let box = NSBox(); box.boxType = .separator; return box }

    private func row(_ kind: PermissionKind) -> NSStackView {
        let name = label(kind.title, size: 13, weight: .medium)
        let detail = label(kind.detail, size: 12, color: .secondaryLabelColor)
        let note = label("", size: 11, color: MemQLColor.accent)
        let text = NSStackView(views: [name, detail, note]); text.orientation = .vertical; text.alignment = .leading; text.spacing = 2
        let toggle = MemQLSwitch()
        toggle.setAccessibilityLabel(kind.title)
        toggle.onToggle = { [weak self] on in self?.toggled(kind, on: on) }
        switches[kind] = toggle; notes[kind] = note
        let row = NSStackView(views: [text, NSView(), toggle]); row.alignment = .centerY; row.spacing = 12
        return row
    }

    func update(_ report: PermissionReport?) {
        self.report = report
        // A restart that ends with both on needs no words: the floor says Ready.
        if let result = progress.observe(report) { message = report?.granted() == true ? "" : result }
        let restarting = progress.pid != nil
        if flow.observe(report) {
            if !flow.asking { inSettings = false }
        }
        asking?(inSettings || flow.asking || report?.requestPending == true)
        for kind in PermissionKind.allCases {
            let view = flow.row(kind, report, restarting: restarting)
            switches[kind]?.isOn = view.on
            switches[kind]?.isPending = view.pending
            switches[kind]?.isEnabled = view.enabled && requestingPermission == nil
            switches[kind]?.setAccessibilityHelp(view.note.isEmpty ? nil : view.note)
            notes[kind]?.stringValue = view.note
            notes[kind]?.isHidden = view.note.isEmpty
        }
        let floor = requestingPermission == nil ? flow.floor(report, restarting: restarting)
            : PermissionFloor(state: "Waiting for macOS", busy: true, act: nil)
        state.stringValue = floor.state
        state.textColor = floor.busy ? MemQLColor.accent : .labelColor
        // Hidden, not merely stopped: a stopped spinner still holds its place
        // in the row and indents the words beside it.
        spinner.isHidden = !floor.busy
        if floor.busy { spinner.startAnimation(nil) } else { spinner.stopAnimation(nil) }
        act.isHidden = floor.act == nil
        act.title = floor.act == .restart ? "Restart MemQL" : "Done"
        let choice = flow.setupChoice(report, restarting: restarting)
        setupRow.isHidden = choice == nil
        if let choice { setupSkip.title = choice }
        feedback.stringValue = message
        feedback.isHidden = message.isEmpty
        identity.stringValue = report.map { "Worker \($0.version) · PID \($0.pid)\n\($0.executable ?? "Executable path not reported")" } ?? "Worker unavailable"
        fit()
    }

    /// Request errors belong in Troubleshooting. Only a fresh worker report
    /// can establish a grant; accepting the request cannot.
    func actionFinished(_ error: String?) {
        if error != nil, let kind = requestingPermission { flow.cancel(kind) }
        requestingPermission = nil
        message = error ?? ""
        update(report)
    }
    func restartFinished(_ error: String?) {
        if let error { progress.cancel(); message = error }
        update(report)
    }

    private func toggled(_ kind: PermissionKind, on: Bool) {
        guard requestingPermission == nil,
              let action = PermissionFlow.action(on: on, report: report, restarting: progress.pid != nil),
              let report else { update(report); return }
        message = ""
        // Back to what the worker reports: the person took back an open ask.
        if on == kind.granted(in: report) {
            flow.cancel(kind)
            if !flow.asking { inSettings = false }
            update(report)
            return
        }
        flow.begin(kind, on: on, pid: report.pid)
        switch action {
        case .prompt:
            requestingPermission = kind
            update(report)
            request?(kind.rawValue, report.pid)
        case .settings:
            openSettings(kind)
        }
    }

    // Only a direct Settings gesture reaches this path. Enabling a switch
    // leaves navigation to the native dialog's Open System Settings button.
    private func openSettings(_ kind: PermissionKind) {
        NSWorkspace.shared.open(kind.settingsURL)
        inSettings = true
        update(report)
    }

    @objc private func openAccessibilitySettings() { openSettings(.accessibility) }
    @objc private func openScreenRecordingSettings() { openSettings(.screenRecording) }

    private func systemSettingsClosed() {
        guard inSettings else { return }
        inSettings = false
        update(report)
    }

    /// The window is as tall as what it says, and grows or shrinks from its
    /// top edge so nothing jumps under the pointer.
    private func fit() {
        guard let window, let stack, let content = window.contentView else { return }
        stack.layoutSubtreeIfNeeded()
        let height = ceil(stack.fittingSize.height) + 36 + 24
        guard abs(content.frame.height - height) > 0.5 else { return }
        let top = window.frame.maxY
        window.setContentSize(NSSize(width: Self.width, height: height))
        window.setFrameTopLeftPoint(NSPoint(x: window.frame.minX, y: top))
    }

    /// Offscreen previews only: shows a state without asking the worker or
    /// opening System Settings.
    func preview(asks: [PermissionKind: Bool], troubleshooting open: Bool = false) {
        flow.preview(asks, pid: report?.pid)
        troubleshooting.isHidden = !open
        troubleshootingButton.state = open ? .on : .off
        update(report)
    }

    @objc private func primaryAct() {
        if flow.floor(report, restarting: progress.pid != nil).act == .restart { confirmRestart() } else { close() }
    }
    @objc private func showWorker() { reveal?() }
    @objc private func skipForNow() {
        guard flow.setupChoice(report, restarting: progress.pid != nil) != nil else { return }
        message = ""
        skip?()
    }
    @objc private func toggleTroubleshooting() {
        troubleshooting.isHidden.toggle()
        troubleshootingButton.state = troubleshooting.isHidden ? .off : .on
        fit()
    }
    @objc private func confirmRestart() {
        guard let report, report.fresh() else { return }
        let alert = NSAlert()
        alert.messageText = "Restart MemQL?"
        alert.informativeText = "Work in progress stops and every server reconnects. macOS checks the permissions again when it starts."
        alert.addButton(withTitle: "Restart"); alert.addButton(withTitle: "Cancel")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        // Keep requests visible until the replacement worker reports.
        inSettings = false
        progress.begin(pid: report.pid)
        update(self.report)
        restart?(report.pid)
    }

    // Requests outlive the window so reopening it still offers the restart.
    func windowWillClose(_ notification: Notification) {
        inSettings = false
        closed?()
    }
}
