import AppKit
import Foundation
import CoreServices

func registerContainingBundles() -> Bool {
    let helper = Bundle.main.bundleURL
    let outer = helper.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    guard Bundle(url: outer)?.bundleIdentifier == "com.znasllc.memql-worker" else { return false }
    return LSRegisterURL(outer as CFURL, true) == noErr && LSRegisterURL(helper as CFURL, true) == noErr
}

struct HomeStatus: Decodable {
    let id: String
    let server: String
    let enabled: Bool
    let state: String
    let os_url: String?
}
struct WorkerStatus: Decodable {
    let version: String
    let pid: Int
    let homes: [HomeStatus]
    let accessibility: String
    let screen_recording: String
    let checked_at: String
    let executable: String?
    let bundle_id: String?
    let bundle_path: String?
    let permission_requests: Bool?
    let permission_request_pending: Bool?
    let setup: SetupStatus?

    var permissionReport: PermissionReport {
        let parser = ISO8601DateFormatter()
        parser.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        var checked = parser.date(from: checked_at)
        if checked == nil { parser.formatOptions = [.withInternetDateTime]; checked = parser.date(from: checked_at) }
        return PermissionReport(pid: pid, version: version, executable: executable,
                                accessibility: accessibility, screenRecording: screen_recording,
                                requestsSupported: permission_requests == true,
                                requestPending: permission_request_pending == true, checkedAt: checked,
                                setupWaiting: setup?.waitingForPermissions == true)
    }
}
struct LogEntry: Decodable {
    let time: String
    let level: String
    let home: String
    let message: String
}
struct Reply: Decodable {
    let ok: Bool
    let error: String?
    let status: WorkerStatus?
    let logs: [LogEntry]?
}

final class CockpitApp: NSObject, NSApplicationDelegate, NSMenuDelegate, NSSearchFieldDelegate, NSWindowDelegate {
    private var item: NSStatusItem!
    private var snapshot: WorkerStatus?
    private var workerError = "Connecting to background worker…"
    private var busy = false
    private var logsBusy = false
    private var trackingMenu = false
    private var pendingHomes: Set<String> = []
    private var timer: Timer?
    private var fastTimer: Timer?
    private var permissionWindow: PermissionSetupWindow?
    private var restartBusy = false
    private var openPermissionsAfterLaunch = false
    /// Setup was already waiting on the Permissions window at the last
    /// refresh: the window opens itself once when a wait begins, and a person
    /// who then closes it is not followed.
    private var setupWasWaiting = false
    private var logWindow: NSWindow?
    private var logText: NSTextView!
    private var search: NSSearchField!
    private var levelFilter: NSPopUpButton!
    private var homeFilter: NSPopUpButton!
    private var follow: NSButton!
    private var logSummary: NSTextField!
    private var logs: [LogEntry] = []
    private var logFailure: String?
    private var following = true
    private let timeParser = ISO8601DateFormatter()
    private let timeDisplay: DateFormatter = {
        let f = DateFormatter(); f.dateFormat = "MMM d HH:mm:ss"; return f
    }()

    func applicationDidFinishLaunching(_ notification: Notification) {
        _ = registerContainingBundles()
        NSApp.setActivationPolicy(.accessory)
        item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        item.button?.image = MarkParser.image()
        item.button?.title = ""
        item.button?.imagePosition = .imageOnly
        item.button?.setAccessibilityLabel("MemQL")
        installApplicationMenu()
        rebuildMenu()
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            self?.refresh()
            if self?.following == true, self?.logWindow?.isVisible == true { self?.refreshLogs() }
        }
        if openPermissionsAfterLaunch { showPermissions() }
    }

    // Opening the installed app again should reveal a useful window even
    // though its normal login presence is only in the menu bar.
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if snapshot?.permissionReport.granted() == true { showLogs() } else { showPermissions() }
        return true
    }
    func application(_ application: NSApplication, open urls: [URL]) {
        guard urls.contains(where: PermissionBridge.accepts) else { return }
        if item == nil { openPermissionsAfterLaunch = true } else { showPermissions() }
    }
    private func installApplicationMenu() {
        let main = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu(title: "MemQL")
        appMenu.addItem(entry("Show Cockpit Menu", action: #selector(showCockpitMenu)))
        appMenu.addItem(entry("Show Logs…", action: #selector(showLogs)))
        appMenu.addItem(entry("Set Up Permissions…", action: #selector(showPermissions)))
        appMenu.addItem(.separator())
        appMenu.addItem(entry("Quit menu bar — worker keeps running", action: #selector(quitMenu)))
        appItem.submenu = appMenu
        main.addItem(appItem)
        let editItem = NSMenuItem()
        let edit = NSMenu(title: "Edit")
        edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit; main.addItem(editItem)
        NSApp.mainMenu = main
    }
    @objc private func showCockpitMenu() { item.button?.performClick(nil) }

    // Follow the worker service's selected executable for both system and
    // user-local installs. Never search PATH or start a second worker.
    // Inside MemQL.app the worker is the app's own executable, which also
    // registers the service (memql-cockpit#545), so there is no plist to read.
    private func workerExecutableURL() -> URL {
        let app = Bundle.main.bundleURL.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        if app.pathExtension == "app", Bundle(url: app)?.bundleIdentifier == "com.znasllc.memql-worker" {
            return app.appendingPathComponent("Contents/MacOS/MemQL")
        }
        let home = FileManager.default.homeDirectoryForCurrentUser
        let plist = home.appendingPathComponent("Library/LaunchAgents/com.znasllc.memql-worker.plist")
        if let data = try? Data(contentsOf: plist),
           let config = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
           let args = config["ProgramArguments"] as? [String],
           let path = args.first, path.hasPrefix("/") {
            return URL(fileURLWithPath: path)
        }
        return home.appendingPathComponent(".memql/bin/memql")
    }

    // This CLI only speaks the owner-only Unix socket. It never starts a
    // worker or loads credentials into the app. Explicit permission requests
    // ask the existing worker to invoke macOS; this CLI does not probe grants.
    private func request(_ args: [String], done: @escaping (Reply?, String?) -> Void) {
        let executable = workerExecutableURL()
        DispatchQueue.global(qos: .userInitiated).async {
            let process = Process()
            let pipe = Pipe()
            process.executableURL = executable
            process.arguments = ["worker", "control"] + args
            process.standardOutput = pipe
            process.standardError = FileHandle.nullDevice
            do {
                try process.run()
                let data = pipe.fileHandleForReading.readDataToEndOfFile()
                process.waitUntilExit()
                let reply = try JSONDecoder().decode(Reply.self, from: data)
                DispatchQueue.main.async { done(reply, reply.error) }
            } catch {
                DispatchQueue.main.async { done(nil, "Background worker unavailable. Check that the MemQL worker service is running.") }
            }
        }
    }

    private func refresh() {
        if busy { return }
        busy = true
        request(["--action=status"]) { [weak self] reply, error in
            guard let self else { return }
            self.busy = false
            self.snapshot = reply?.ok == true ? reply?.status : nil
            self.workerError = error ?? "Background worker unavailable"
            self.item.button?.toolTip = self.snapshot.map { "MemQL · \($0.homes.filter { $0.state == "Connected" }.count) connected" } ?? "MemQL · worker unavailable"
            if !self.trackingMenu { self.rebuildMenu() }
            self.permissionWindow?.update(self.snapshot?.permissionReport)
            // Setup waits on the person here (memql-cockpit#548): the window
            // comes forward when a wait begins, even if the installer could
            // not open it.
            let setupWaiting = self.snapshot?.setup?.waitingForPermissions == true
            if setupWaiting && !self.setupWasWaiting && self.permissionWindow?.window?.isVisible != true { self.showPermissions() }
            self.setupWasWaiting = setupWaiting
            let setupKey = "permissionSetupShown.\(self.snapshot?.bundle_id ?? "standalone").\(self.snapshot?.version ?? "unknown")"
            if let report = self.snapshot?.permissionReport, report.fresh(), report.requestsSupported,
               !report.granted(), !UserDefaults.standard.bool(forKey: setupKey) {
                UserDefaults.standard.set(true, forKey: setupKey)
                self.showPermissions()
            }
        }
    }
    func menuWillOpen(_ menu: NSMenu) { trackingMenu = true; refresh() }
    func menuDidClose(_ menu: NSMenu) { trackingMenu = false; rebuildMenu() }
    private func entry(_ title: String, action: Selector? = nil, payload: Any? = nil) -> NSMenuItem {
        let entry = NSMenuItem(title: title, action: action, keyEquivalent: "")
        entry.target = self; entry.representedObject = payload
        return entry
    }
    private func heading(_ title: String) -> NSMenuItem {
        let item = entry(title); item.isEnabled = false; return item
    }
    private func rebuildMenu() {
        let menu = NSMenu(); menu.delegate = self; menu.autoenablesItems = false
        menu.addItem(heading("MemQL"))
        if let snapshot {
            menu.addItem(heading("Worker running · \(snapshot.homes.filter { $0.state == "Connected" }.count) of \(snapshot.homes.count) servers connected"))
            menu.addItem(.separator())
            for home in snapshot.homes {
                let server = entry("\(home.server) — \(home.state)")
                let submenu = NSMenu(); submenu.autoenablesItems = false
                submenu.addItem(heading(home.state == "Paused" ? "Connections paused on this Mac" : "\(home.state) · this Mac"))
                let toggle = entry(home.enabled ? "Pause connection" : "Allow connecting", action: #selector(toggleHome(_:)), payload: home.id)
                toggle.isEnabled = !pendingHomes.contains(home.id)
                submenu.addItem(toggle)
                if let osURL = home.os_url, !osURL.isEmpty {
                    submenu.addItem(entry("Open MemQL OS", action: #selector(openOS(_:)), payload: osURL))
                }
                server.submenu = submenu; menu.addItem(server)
            }
            menu.addItem(.separator())
            let permissions = entry("macOS permissions")
            let sub = NSMenu(); sub.autoenablesItems = false
            sub.addItem(heading("Background worker · current process"))
            sub.addItem(entry("Set Up Permissions…", action: #selector(showPermissions)))
            sub.addItem(entry("Accessibility: \(snapshot.accessibility)", action: #selector(openAccessibility)))
            sub.addItem(entry("Screen Recording: \(snapshot.screen_recording)", action: #selector(openScreenRecording)))
            sub.addItem(entry("Show installed worker in Finder", action: #selector(revealWorker)))
            sub.addItem(heading("Rechecks automatically; macOS may require a worker restart."))
            permissions.submenu = sub; menu.addItem(permissions)
            menu.addItem(entry("Show Logs…", action: #selector(showLogs)))
            menu.addItem(heading("Worker \(snapshot.version) · PID \(snapshot.pid)"))
        } else {
            menu.addItem(heading(workerError))
            menu.addItem(entry("Retry worker connection", action: #selector(retry)))
            menu.addItem(entry("Set Up Permissions…", action: #selector(showPermissions)))
            menu.addItem(entry("Show Logs…", action: #selector(showLogs)))
        }
        menu.addItem(.separator())
        menu.addItem(entry("Quit menu bar — worker keeps running", action: #selector(quitMenu)))
        item.menu = menu
    }
    @objc private func retry() { refresh(); refreshLogs() }
    @objc private func toggleHome(_ sender: NSMenuItem) {
        guard let id = sender.representedObject as? String,
              let home = snapshot?.homes.first(where: { $0.id == id }) else { return }
        pendingHomes.insert(id)
        request(["--action=home", "--home=\(id)", "--enabled=\(!home.enabled)"]) { [weak self] reply, error in
            guard let self else { return }
            self.pendingHomes.remove(id)
            if reply?.ok == true { self.snapshot = reply?.status }
            else { self.alert("Connection policy was not changed", error ?? "The worker did not accept the change.") }
            self.refresh()
        }
    }
    @objc private func openOS(_ sender: NSMenuItem) {
        guard let raw = sender.representedObject as? String, let url = URL(string: raw), ["https", "http"].contains(url.scheme ?? "") else { return }
        NSWorkspace.shared.open(url)
    }
    @objc private func openAccessibility() { NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility")!) }
    @objc private func openScreenRecording() { NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture")!) }
    @objc private func revealWorker() {
        let url: URL
        if snapshot?.bundle_id == "com.znasllc.memql-worker", let path = snapshot?.bundle_path {
            url = URL(fileURLWithPath: path)
        } else { url = workerExecutableURL().resolvingSymlinksInPath() }
        NSWorkspace.shared.activateFileViewerSelecting([url])
    }
    @objc private func quitMenu() { NSApp.terminate(nil) }

    @objc private func showPermissions() {
        if permissionWindow == nil {
            let window = PermissionSetupWindow()
            window.request = { [weak self] permission, pid in
                self?.request(["--action=request-permission", "--permission=\(permission)", "--pid=\(pid)"]) { reply, error in
                    self?.permissionWindow?.actionFinished(reply?.ok == true ? nil : (error ?? "The worker did not accept the request."))
                    self?.refresh()
                }
            }
            window.reveal = { [weak self] in self?.revealWorker() }
            // The person's choice goes to the worker as an answer; the
            // installer's own setup command reports how the stage ended.
            window.skip = { [weak self] in
                self?.request(["--action=setup", "--stage=permissions", "--answer=skip"]) { reply, error in
                    if reply?.ok == true { self?.permissionWindow?.close() } else {
                        self?.permissionWindow?.actionFinished(error ?? "Setup did not take that choice. Try again.")
                    }
                    self?.refresh()
                }
            }
            window.restart = { [weak self] pid in self?.restartWorker(expectedPID: pid) }
            window.asking = { [weak self] asking in self?.setFastRefresh(asking) }
            // Asked once the window has gone: it is still visible while it closes.
            window.closed = { [weak self] in DispatchQueue.main.async { self?.windowsChanged() } }
            permissionWindow = window
        }
        permissionWindow?.update(snapshot?.permissionReport)
        showInDock()
        bringForward(permissionWindow?.window)
        refresh()
    }

    // A WINDOW OF OURS IS NEVER LOST (memql-cockpit#543). This helper lives in
    // the menu bar with no Dock icon and no Command-Tab entry, so a window of
    // it fell behind System Settings, and behind whatever came forward after
    // Settings quit, with nothing on screen leading back. While one is open
    // MemQL is an ordinary app; when the last one closes it is a menu bar
    // item again.
    private func showInDock() {
        if NSApp.activationPolicy() != .regular { NSApp.setActivationPolicy(.regular) }
    }
    // macOS may decline a background helper's request to become active, which
    // left a window that opened by itself after an install behind the app in
    // front. Ordered front regardless, it is on top either way.
    private func bringForward(_ window: NSWindow?) {
        NSApp.activate(ignoringOtherApps: true)
        window?.makeKeyAndOrderFront(nil)
        window?.orderFrontRegardless()
    }
    private func windowsChanged() {
        let open = [permissionWindow?.window, logWindow].contains { $0?.isVisible == true }
        if !open && NSApp.activationPolicy() != .accessory { NSApp.setActivationPolicy(.accessory) }
    }
    func windowWillClose(_ notification: Notification) {
        // Runs before the window is ordered out, so look again once it is.
        DispatchQueue.main.async { [weak self] in self?.windowsChanged() }
    }

    // While the person is in System Settings the worker's report is read
    // every second, so the switch comes on as soon as macOS grants.
    private func setFastRefresh(_ on: Bool) {
        guard on != (fastTimer != nil) else { return }
        fastTimer?.invalidate(); fastTimer = nil
        if on { fastTimer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in self?.refresh() } }
    }

    // Only the confirmed button reaches this method. Resolve the installed
    // service and verify its loaded PID/program before targeting its label.
    private func restartWorker(expectedPID: Int) {
        guard !restartBusy, let report = snapshot?.permissionReport, report.fresh(),
              report.pid == expectedPID, let actualPath = report.executable else {
            permissionWindow?.restartFinished("The worker changed. Wait for its current status and try again.")
            return
        }
        let executable = workerExecutableURL()
        guard executable.resolvingSymlinksInPath() == URL(fileURLWithPath: actualPath).resolvingSymlinksInPath() else {
            permissionWindow?.restartFinished("The running worker differs from the installed service. Reinstall Cockpit to repair the service.")
            return
        }
        restartBusy = true
        DispatchQueue.global(qos: .userInitiated).async {
            func launchctl(_ arguments: [String]) throws -> (Int32, String) {
                let task = Process(); let pipe = Pipe()
                task.executableURL = URL(fileURLWithPath: "/bin/launchctl")
                task.arguments = arguments; task.standardOutput = pipe; task.standardError = FileHandle.nullDevice
                try task.run()
                let bytes = pipe.fileHandleForReading.readDataToEndOfFile(); task.waitUntilExit()
                return (task.terminationStatus, String(data: bytes, encoding: .utf8) ?? "")
            }
            var failure: String?
            do {
                let service = "gui/\(getuid())/com.znasllc.memql-worker"
                let (status, description) = try launchctl(["print", service])
                if status != 0 || !PermissionBridge.serviceMatches(description, pid: expectedPID, executable: executable.path) {
                    failure = "The managed worker changed or is unavailable. Refresh its status before restarting."
                } else if try launchctl(["kickstart", "-k", service]).0 != 0 {
                    failure = "macOS could not restart the worker. Open Cockpit Logs to check the service."
                }
            } catch { failure = "Unable to contact the macOS worker service." }
            DispatchQueue.main.async {
                self.restartBusy = false
                self.permissionWindow?.restartFinished(failure)
                self.refresh()
            }
        }
    }
    private func alert(_ title: String, _ message: String) {
        let alert = NSAlert(); alert.messageText = title; alert.informativeText = message; alert.runModal()
    }

    @objc private func showLogs() {
        if logWindow == nil { makeLogWindow() }
        showInDock()
        bringForward(logWindow)
        refreshLogs()
    }
    private func makeLogWindow() {
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 960, height: 580), styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = "MemQL — Worker Logs"
        window.minSize = NSSize(width: 650, height: 360)
        window.isReleasedWhenClosed = false
        window.delegate = self
        let content = window.contentView!
        search = NSSearchField(); search.placeholderString = "Search worker logs"; search.delegate = self
        levelFilter = NSPopUpButton(); levelFilter.addItems(withTitles: ["All levels", "INFO", "WARN", "ERROR", "DEBUG"])
        levelFilter.target = self; levelFilter.action = #selector(filtersChanged)
        homeFilter = NSPopUpButton(); homeFilter.addItem(withTitle: "All servers")
        homeFilter.target = self; homeFilter.action = #selector(filtersChanged)
        follow = NSButton(title: "Pause follow", target: self, action: #selector(toggleFollow))
        follow.bezelStyle = .rounded
        let refreshButton = NSButton(title: "Refresh", target: self, action: #selector(refreshLogs)); refreshButton.bezelStyle = .rounded
        let toolbar = NSStackView(views: [search, homeFilter, levelFilter, follow, refreshButton])
        toolbar.orientation = .horizontal; toolbar.spacing = 8
        let scroll = NSScrollView(); scroll.hasVerticalScroller = true; scroll.hasHorizontalScroller = false; scroll.borderType = .bezelBorder
        logText = NSTextView(frame: NSRect(x: 0, y: 0, width: 900, height: 480))
        logText.isEditable = false; logText.isSelectable = true
        logText.font = NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
        logText.textContainerInset = NSSize(width: 12, height: 10)
        logText.isHorizontallyResizable = false; logText.isVerticallyResizable = true
        logText.autoresizingMask = [.width]
        logText.textContainer?.widthTracksTextView = true
        scroll.documentView = logText
        logSummary = NSTextField(labelWithString: "Local worker diagnostics · credentials redacted · no uploads")
        logSummary.font = NSFont.systemFont(ofSize: 11); logSummary.textColor = .secondaryLabelColor
        for view in [toolbar, scroll, logSummary!] { view.translatesAutoresizingMaskIntoConstraints = false; content.addSubview(view) }
        NSLayoutConstraint.activate([
            toolbar.topAnchor.constraint(equalTo: content.topAnchor, constant: 16), toolbar.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 16), toolbar.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -16),
            search.widthAnchor.constraint(greaterThanOrEqualToConstant: 180), homeFilter.widthAnchor.constraint(lessThanOrEqualToConstant: 220),
            scroll.topAnchor.constraint(equalTo: toolbar.bottomAnchor, constant: 12), scroll.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 16), scroll.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -16), scroll.bottomAnchor.constraint(equalTo: logSummary.topAnchor, constant: -12),
            logSummary.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 16), logSummary.bottomAnchor.constraint(equalTo: content.bottomAnchor, constant: -12)
        ])
        window.center(); logWindow = window
    }
    @objc private func refreshLogs() {
        if logWindow == nil || logsBusy { return }
        logsBusy = true
        request(["--action=logs"]) { [weak self] reply, error in
            guard let self else { return }
            self.logsBusy = false
            if reply?.ok == true { self.logs = reply?.logs ?? []; self.logFailure = nil }
            else { self.logFailure = error ?? "Worker log unavailable" }
            let selected = self.homeFilter.titleOfSelectedItem
            let homes = Array(Set(self.logs.map(\.home).filter { !$0.isEmpty })).sorted()
            self.homeFilter.removeAllItems(); self.homeFilter.addItems(withTitles: ["All servers"] + homes)
            if let selected, self.homeFilter.itemTitles.contains(selected) { self.homeFilter.selectItem(withTitle: selected) }
            self.renderLogs()
        }
    }
    @objc private func toggleFollow() { following.toggle(); follow.title = following ? "Pause follow" : "Resume follow"; if following { refreshLogs() }; renderLogs() }
    @objc private func filtersChanged() { renderLogs() }
    func controlTextDidChange(_ notification: Notification) { renderLogs() }
    private func renderLogs() {
        guard logText != nil else { return }
        let needle = search.stringValue.lowercased()
        let level = levelFilter.titleOfSelectedItem ?? "All levels"
        let home = homeFilter.titleOfSelectedItem ?? "All servers"
        let visible = logs.filter { entry in
            (level == "All levels" || entry.level == level) && (home == "All servers" || entry.home == home) && (needle.isEmpty || "\(entry.time) \(entry.level) \(entry.home) \(entry.message)".lowercased().contains(needle))
        }
        let text = NSMutableAttributedString(string: "")
        for entry in visible {
            timeParser.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            var date = timeParser.date(from: entry.time)
            if date == nil { timeParser.formatOptions = [.withInternetDateTime]; date = timeParser.date(from: entry.time) }
            let stamp = date.map(timeDisplay.string) ?? entry.time
            let color: NSColor = entry.level == "ERROR" ? .systemRed : entry.level == "WARN" ? .systemOrange : .labelColor
            let line = "\(stamp)  \(entry.level.padding(toLength: 5, withPad: " ", startingAt: 0))  \(entry.home.isEmpty ? "worker" : entry.home)\n\(entry.message)\n\n"
            text.append(NSAttributedString(string: line, attributes: [.font: NSFont.monospacedSystemFont(ofSize: 12, weight: .regular), .foregroundColor: color]))
        }
        if visible.isEmpty { text.append(NSAttributedString(string: logFailure ?? "No matching worker log entries.")) }
        let oldPosition = logText.enclosingScrollView?.contentView.bounds.origin ?? .zero
        logText.textStorage?.setAttributedString(text)
        if following { logText.scrollToEndOfDocument(nil) }
        else { logText.enclosingScrollView?.contentView.scroll(to: oldPosition) }
        logSummary.stringValue = logFailure ?? "\(visible.count) entries · latest 300 · \(following ? "following" : "follow paused") · local only · credentials redacted"
    }
}

// Register both real bundles without showing UI or starting the worker.
// The app installer invokes this after placing the containing bundle.
if CommandLine.arguments.contains("--register-bundles") {
    guard registerContainingBundles() else {
        fputs("MemQL bundle registration failed.\n", stderr); exit(1)
    }
    exit(0)
}

// Build-time icon generation uses the same canonical vector geometry as the
// menu mark, rendered at every native icon resolution.
if let arg = CommandLine.arguments.first(where: { $0.hasPrefix("--write-iconset=") }) {
    let directory = URL(fileURLWithPath: String(arg.dropFirst("--write-iconset=".count)))
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    for size in [16, 32, 128, 256, 512] {
        for factor in [1, 2] {
            let pixels = size * factor
            guard let mark = MemQLAppIcon.image(pixels: pixels, points: size),
                  let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels,
                    bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                    colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0),
                  let context = NSGraphicsContext(bitmapImageRep: bitmap) else { exit(1) }
            NSGraphicsContext.saveGraphicsState(); NSGraphicsContext.current = context
            mark.draw(in: NSRect(x: 0, y: 0, width: pixels, height: pixels))
            NSGraphicsContext.restoreGraphicsState()
            let name = "icon_\(size)x\(size)\(factor == 2 ? "@2x" : "").png"
            try bitmap.representation(using: .png, properties: [:])!.write(to: directory.appendingPathComponent(name))
        }
    }
    exit(0)
}

// Packaging checks the real bundled resource and rasterization, not merely
// whether an SVG file was copied. A malformed icon must fail the build.
if CommandLine.arguments.contains("--check-icon") {
    guard let icon = MarkParser.image(), icon.isTemplate,
          let tiff = icon.tiffRepresentation,
          let bitmap = NSBitmapImageRep(data: tiff) else {
        fputs("MemQL menu icon could not be parsed or rendered.\n", stderr)
        exit(1)
    }
    var visiblePixels = 0
    for y in 0..<bitmap.pixelsHigh {
        for x in 0..<bitmap.pixelsWide {
            if (bitmap.colorAt(x: x, y: y)?.alphaComponent ?? 0) > 0 { visiblePixels += 1 }
        }
    }
    guard visiblePixels > 0, icon.size == NSSize(width: 20, height: 20) else {
        fputs("MemQL menu icon is empty or has an unexpected size.\n", stderr)
        exit(1)
    }
    print("MemQL icon verified: 20pt template, \(visiblePixels) visible pixels, canonical nine-node mark.")
    exit(0)
}

let app = NSApplication.shared
let delegate = CockpitApp()
app.delegate = delegate
app.run()
