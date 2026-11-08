import AppKit

@main
enum CockpitUpdateTests {
    static func reply(_ json: String) -> CockpitUpdateReply {
        try! JSONDecoder().decode(CockpitUpdateReply.self, from: Data(json.utf8))
    }
    static func main() throws {
        let available = reply(#"{"ok":true,"current_version":"0.22.2+g1234567","latest_version":"0.23.0","available":true,"can_install":true}"#)
        let current = reply(#"{"ok":true,"current_version":"0.23.0","latest_version":"0.23.0","available":false,"can_install":true}"#)
        precondition(available.processError(exitStatus: 0) == nil)
        precondition(available.processError(exitStatus: 1) != nil, "An ok envelope cannot hide a failed child process")
        let refused = reply(#"{"ok":false,"error":"Worker is busy"}"#)
        precondition(refused.processError(exitStatus: 1) == "Worker is busy")
        precondition(reply(#"{"ok":false}"#).processError(exitStatus: 0) != nil)
        var state = CockpitUpdateState()
        precondition(state.action == .check && !state.hasUpdate)
        precondition(state.beginCheck())
        precondition(!state.beginCheck() && state.action == nil, "Checks cannot overlap")
        precondition(state.beginInstall() == nil, "No installation without a checked available release")
        state.checked(available, error: nil)
        precondition(state.hasUpdate && state.action == .install)
        var failedCheck = CockpitUpdateState()
        _ = failedCheck.beginCheck()
        failedCheck.checked(available, error: available.processError(exitStatus: 1))
        precondition(!failedCheck.hasUpdate && failedCheck.failure != nil, "A failed child cannot advertise an unconfirmed update")
        precondition(state.beginCheck())
        state.checked(nil, error: "Offline")
        precondition(state.hasUpdate && state.action == .install, "A network failure cannot hide a known update")
        precondition(state.failure == "Offline")
        precondition(state.beginInstall() == "0.23.0")
        precondition(state.action == nil && !state.beginCheck() && state.beginInstall() == nil, "Only one install/check at a time")
        for phase in ["downloading", "verifying", "installing", "restarting", "reconnecting"] {
            state.progress(reply("{\"ok\":true,\"phase\":\"\(phase)\"}"))
            precondition(state.phase == phase && state.installing)
        }
        state.progress(reply(#"{"ok":true,"phase":"complete"}"#))
        precondition(state.installing, "A polled receipt cannot finish the still-running install command")
        state.finished(nil, error: "Checksum verification failed")
        precondition(state.hasUpdate && state.action == .install && !state.installing)
        precondition(state.failure == "Checksum verification failed")
        precondition(state.beginInstall() == "0.23.0")
        let disconnected = reply(#"{"ok":true,"updated":true,"worker_reconnected":false,"current_version":"0.23.0","latest_version":"0.23.0","phase":"complete","message":"Cockpit updated. One server is still offline."}"#)
        state.finished(disconnected, error: nil)
        precondition(!state.hasUpdate && state.action == .check)
        precondition(state.title.contains("pending") && state.detail.contains("offline"), "Install success is not reconnection success")
        precondition(state.beginCheck())
        state.checked(current, error: nil)
        precondition(state.title.contains("pending"), "A release check does not prove worker reconnection")
        state.observeConnections(workerVersion: "0.22.2", enabled: 1, connected: 1)
        precondition(state.title.contains("pending"), "The old worker cannot prove new-version reconnection")
        state.observeConnections(workerVersion: "0.23.0+g1234567", enabled: 2, connected: 1)
        precondition(state.title.contains("pending"), "Partial reconnection remains pending")
        state.observeConnections(workerVersion: "0.23.0+g1234567", enabled: 1, connected: 1)
        precondition(state.title == "Cockpit updated and connected")
        let connected = reply(#"{"ok":true,"updated":true,"worker_reconnected":true,"current_version":"0.23.0","phase":"complete"}"#)
        state.finished(connected, error: nil)
        precondition(state.title == "Cockpit updated and connected")
        var partial = CockpitUpdateState()
        partial.finished(connected, error: connected.processError(exitStatus: 1))
        precondition(partial.failure != nil && partial.action == .check && !partial.hasUpdate,
                     "A replaced worker plus process failure remains a partial outcome, not another install")
        state.progress(reply(#"{"ok":true,"phase":"downloading"}"#))
        precondition(!state.installing && state.title == "Cockpit updated and connected", "Late progress cannot undo completion")

        var unsupported = available
        unsupported.can_install = false
        unsupported.message = "Install Cockpit in your Applications folder to enable updates."
        state = CockpitUpdateState(); state.checked(unsupported, error: nil)
        precondition(state.hasUpdate && state.action == .check && state.beginInstall() == nil)
        precondition(state.detail == unsupported.message)
        let recovery = reply(#"{"ok":true,"current_version":"0.23.0","latest_version":"0.23.0","available":false,"restart_required":true,"can_install":true,"message":"Restart the worker to finish installing Cockpit 0.23.0"}"#)
        state = CockpitUpdateState(); state.checked(recovery, error: nil)
        precondition(state.hasUpdate && state.action == .install, "Interrupted replacement must retain the badge and recovery action")
        precondition(state.actionTitle == "Restart to Finish Update")
        precondition(state.beginCheck())
        state.checked(nil, error: "Offline")
        precondition(state.hasUpdate && state.action == .install, "An offline check must not hide a known restart requirement")
        precondition(state.beginInstall() == "0.23.0", "Recovery targets the already installed version")
        state.progress(reply(#"{"ok":true,"phase":"restarting","restart_required":true}"#))
        precondition(state.installing && state.title == "Restarting the worker…")
        state.finished(connected, error: nil)
        precondition(!state.hasUpdate && state.action == .check, "A confirmed recovery removes the restart badge")
        precondition(reply(#"{"ok":true,"updated_at":"2026-11-07T12:34:56Z"}"#).timestamp != nil)
        precondition(reply(#"{"ok":true,"updated_at":"2026-11-07T12:34:56.123Z"}"#).timestamp != nil)
        precondition(reply(#"{"ok":true,"updated_at":"invalid"}"#).timestamp == nil)

        // Exercise the actual control: the pending state cannot invoke its
        // action, and keyboard activation returns once an action is legal.
        let app = NSApplication.shared; app.setActivationPolicy(.prohibited)
        let controls = CockpitAboutWindow()
        var presses = 0
        controls.action = { presses += 1 }
        state = CockpitUpdateState(); state.checked(available, error: nil)
        controls.update(menuVersion: "0.22.2", workerVersion: nil, connectionSummary: "Background worker unavailable", state: state)
        let actionButton = controls.window!.contentView!.subviews.compactMap { $0 as? NSButton }.first!
        precondition(actionButton.keyEquivalent == "\r" && actionButton.isEnabled)
        actionButton.performClick(nil)
        precondition(presses == 1)
        _ = state.beginInstall()
        controls.update(menuVersion: "0.22.2", workerVersion: "0.22.2", connectionSummary: "1 of 2 servers connected", state: state)
        actionButton.performClick(nil)
        precondition(!actionButton.isEnabled && presses == 1)
        state.finished(nil, error: "Update unavailable; try again.")
        controls.update(menuVersion: "0.22.2", workerVersion: "0.22.2", connectionSummary: "1 of 2 servers connected", state: state)
        precondition(actionButton.isEnabled)
        controls.window!.contentView!.layoutSubtreeIfNeeded()
        precondition(controls.window!.contentView!.bounds.contains(actionButton.frame), "Action must remain inside the window")

        let resource = URL(fileURLWithPath: FileManager.default.currentDirectoryPath).appendingPathComponent("native/macos/mark.svg")
        let plain = CockpitMenuIcon.image(updateAvailable: false, resourceURL: resource)!
        let badge = CockpitMenuIcon.image(updateAvailable: true, resourceURL: resource)!
        precondition(plain.isTemplate && badge.isTemplate, "Both menu icons must follow system contrast")
        precondition(plain.size == NSSize(width: 20, height: 20) && badge.size == NSSize(width: 28, height: 20))
        let bitmap = NSBitmapImageRep(data: badge.tiffRepresentation!)!
        let arrowStart = Int(Double(bitmap.pixelsWide) * 21 / 28)
        var arrowPixels = 0
        for y in 0..<bitmap.pixelsHigh { for x in arrowStart..<bitmap.pixelsWide {
            if (bitmap.colorAt(x: x, y: y)?.alphaComponent ?? 0) > 0.1 { arrowPixels += 1 }
        } }
        precondition(arrowPixels > 0, "The update arrow must actually render beside the mark")

        if let output = CommandLine.arguments.first(where: { $0.hasPrefix("--render=") }) {
            state = CockpitUpdateState(); state.checked(available, error: nil)
            let mode = CommandLine.arguments.first(where: { $0.hasPrefix("--state=") }).map { String($0.dropFirst("--state=".count)) } ?? "available"
            switch mode {
            case "checking": _ = state.beginCheck()
            case "reconnecting": _ = state.beginInstall(); state.progress(reply(#"{"ok":true,"phase":"reconnecting"}"#))
            case "error": state.finished(nil, error: "The update could not be verified. Your installed version is unchanged. Check your connection and try again.")
            case "complete": state.finished(connected, error: nil)
            case "offline": state.finished(disconnected, error: nil)
            case "recovery": state.checked(recovery, error: nil)
            default: break
            }
            let about = CockpitAboutWindow(logo: MarkParser.image(size: 48, template: false, resourceURL: resource))
            about.update(menuVersion: "0.22.2", workerVersion: "0.22.2+g1234567", connectionSummary: "1 of 2 servers connected", state: state)
            let view = about.window!.contentView!
            let appearance = NSAppearance(named: CommandLine.arguments.contains("--dark") ? .darkAqua : .aqua)!
            view.appearance = appearance; view.wantsLayer = true
            appearance.performAsCurrentDrawingAppearance { view.layer?.backgroundColor = NSColor.windowBackgroundColor.cgColor }
            view.layoutSubtreeIfNeeded()
            let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds)!
            appearance.performAsCurrentDrawingAppearance { view.cacheDisplay(in: view.bounds, to: bitmap) }
            try bitmap.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: String(output.dropFirst("--render=".count))))
        }
        print("Cockpit updates: verified release, retry, concurrent actions, progress, disconnected completion and template badge checks passed.")
    }
}
