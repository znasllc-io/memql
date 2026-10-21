import Foundation
import AppKit

@main
enum PermissionSetupTests {
    static func main() {
        let now = Date()
        func report(pid: Int = 100, ax: String = "Granted", screen: String = "Granted", age: TimeInterval = 0, pending: Bool = false) -> PermissionReport {
            PermissionReport(pid: pid, version: "fixture", executable: "/fixture/memql", accessibility: ax,
                             screenRecording: screen, requestsSupported: true, requestPending: pending,
                             checkedAt: now.addingTimeInterval(-age))
        }
        precondition(report().granted(at: now))
        precondition(!report(ax: "Unknown").granted(at: now))
        precondition(!report(screen: "Not granted").granted(at: now))
        precondition(!report(age: 16).granted(at: now))
        precondition(!report(age: -60).granted(at: now))
        var progress = PermissionRestartProgress()
        progress.begin(pid: 100, now: now)
        precondition(!progress.ready(report(), now: now), "Old PID cannot confirm restart")
        precondition(progress.observe(nil, now: now) == nil)
        precondition(!progress.ready(nil, now: now), "Offline cannot mean ready")
        precondition(progress.observe(report(pid: 101, age: 16), now: now) == nil, "Stale report cannot confirm restart")
        let restartResult = progress.observe(report(pid: 101, screen: "Not granted"), now: now)
        precondition(restartResult?.hasPrefix("Restart complete.") == true)
        precondition(restartResult?.contains("Checking") == false, "Completed restart must not leave a transient checking banner")
        precondition(!progress.ready(report(pid: 101, screen: "Not granted"), now: now), "New PID alone cannot grant access")
        precondition(progress.ready(report(pid: 101), now: now))
        progress.begin(pid: 101, now: now)
        precondition(progress.observe(nil, now: now.addingTimeInterval(31)) != nil, "Failed restart must time out")
        precondition(!progress.ready(nil, now: now))
        precondition(PermissionBridge.accepts(URL(string: "memql-cockpit://permissions")!))
        for raw in ["memql-cockpit://permissions?restart=true", "memql-cockpit://permissions/request", "memql-cockpit://restart", "https://permissions", "memql-cockpit://user@permissions", "memql-cockpit://permissions#request", "memql-cockpit://permissions:123"] {
            precondition(!PermissionBridge.accepts(URL(string: raw)!), "URL must be view-only: \(raw)")
        }
        // One switch per permission (memql-cockpit#543): what each row and the
        // floor say, from the worker's report and what the person asked for.
        var flow = PermissionFlow()
        let neither = report(ax: "Not granted", screen: "Not granted")
        let axOnly = report(screen: "Not granted")
        precondition(flow.row(.accessibility, neither, restarting: false, now: now) == PermissionRow(on: false, enabled: true, note: ""))
        precondition(flow.floor(neither, restarting: false, now: now) == PermissionFloor(state: "Turn on both to finish", busy: false, act: nil))
        flow.begin(.accessibility, on: true, pid: 100)
        precondition(flow.row(.accessibility, neither, restarting: false, now: now) == PermissionRow(on: false, enabled: true, note: "Permission setup pending", pending: true),
                     "A request is visually pending, never ordinary off or confirmed on")
        precondition(flow.floor(neither, restarting: false, now: now) == PermissionFloor(state: "After approval, restart MemQL", busy: false, act: .restart))
        precondition(!flow.observe(neither, now: now), "An ask ends only when the worker sees it")
        precondition(!flow.observe(report(age: 16), now: now), "A stale report ends nothing")
        precondition(flow.observe(axOnly, now: now) && !flow.asking)
        precondition(flow.floor(axOnly, restarting: false, now: now) == PermissionFloor(state: "Turn on Screen Recording to finish", busy: false, act: nil))
        flow.begin(.accessibility, on: true, pid: 100)
        flow.cancel(.accessibility)
        precondition(!flow.asking, "The person can cancel a pending request")

        // Screen Recording may need a new worker process. A request stays off
        // until observed, and a restart cannot manufacture a grant.
        flow.begin(.screenRecording, on: true, pid: 100)
        precondition(flow.row(.screenRecording, neither, restarting: false, now: now) == PermissionRow(on: false, enabled: true, note: "Permission setup pending", pending: true))
        precondition(flow.floor(neither, restarting: false, now: now) == PermissionFloor(state: "After approval, restart MemQL", busy: false, act: .restart))
        flow.begin(.accessibility, on: true, pid: 100)
        precondition(flow.floor(neither, restarting: false, now: now) == PermissionFloor(state: "After approval, restart MemQL", busy: false, act: .restart))
        precondition(flow.observe(axOnly, now: now) && flow.asks == [.screenRecording: true], "Accessibility landing leaves the Screen Recording ask open")
        precondition(!flow.observe(axOnly, now: now) && flow.asks == [.screenRecording: true], "A denied report does not complete an outstanding request")
        precondition(flow.floor(axOnly, restarting: false, now: now) == PermissionFloor(state: "After approval, restart MemQL", busy: false, act: .restart))
        precondition(flow.floor(axOnly, restarting: true, now: now) == PermissionFloor(state: "Restarting MemQL…", busy: true, act: nil))
        precondition(!flow.row(.screenRecording, axOnly, restarting: true, now: now).enabled, "Nothing is asked while MemQL restarts")
        precondition(flow.observe(report(pid: 101, screen: "Not granted"), now: now) && !flow.asking,
                     "A new worker's report settles it either way")
        precondition(flow.row(.screenRecording, report(pid: 101, screen: "Not granted"), restarting: false, now: now) == PermissionRow(on: false, enabled: true, note: ""))
        flow.begin(.screenRecording, on: true, pid: 101)
        precondition(flow.observe(report(pid: 101), now: now), "A grant the running worker does see settles it too")
        precondition(flow.floor(report(pid: 101), restarting: false, now: now) == PermissionFloor(state: "Ready", busy: false, act: .done))
        flow.begin(.screenRecording, on: false, pid: 101)
        precondition(flow.floor(report(pid: 101), restarting: false, now: now).act == .restart, "Turning it off waits for the restart too, not Ready")
        flow.cancel(.screenRecording)
        precondition(!flow.asking)

        flow.begin(.accessibility, on: false, pid: 100)
        precondition(flow.row(.accessibility, report(), restarting: false, now: now).note == "Permission setup pending")
        precondition(!flow.observe(report(), now: now) && flow.observe(report(ax: "Not granted"), now: now),
                     "Turning off ends when the worker reports it off")
        precondition(!flow.row(.accessibility, report(age: 16), restarting: false, now: now).enabled, "No switch without a fresh report")
        precondition(flow.floor(nil, restarting: false, now: now) == PermissionFloor(state: "Waiting for the worker", busy: true, act: nil))
        let headless = PermissionReport(pid: 100, version: "fixture", executable: nil, accessibility: "Not granted", screenRecording: "Not granted",
                                        requestsSupported: false, requestPending: false, checkedAt: now)
        precondition(!flow.row(.screenRecording, headless, restarting: false, now: now).enabled)
        precondition(flow.floor(headless, restarting: false, now: now) == PermissionFloor(state: "Update Cockpit to set these up", busy: false, act: nil))

        // Enabling requests only the native prompt; Settings is a separate
        // direct action (also used to revoke access). Both controls share this.
        precondition(PermissionFlow.action(on: true, report: neither, restarting: false, now: now) == .prompt)
        precondition(PermissionFlow.action(on: false, report: report(), restarting: false, now: now) == .settings)
        for blocked in [nil, report(age: 16), headless, report(pending: true)] as [PermissionReport?] {
            precondition(PermissionFlow.action(on: true, report: blocked, restarting: false, now: now) == nil)
        }
        precondition(PermissionFlow.action(on: true, report: neither, restarting: true, now: now) == nil)
        let pending = report(ax: "Not granted", screen: "Not granted", pending: true)
        for kind in PermissionKind.allCases {
            precondition(!flow.row(kind, pending, restarting: false, now: now).enabled,
                         "No second request while macOS holds a dialog open")
            precondition(!flow.row(kind, pending, restarting: false, now: now).on)
        }
        flow.begin(.screenRecording, on: true, pid: 100)
        precondition(flow.floor(pending, restarting: false, now: now) == PermissionFloor(state: "Waiting for macOS", busy: true, act: nil),
                     "Do not offer a restart while macOS is still processing the request")
        precondition(flow.row(.accessibility, report(), restarting: false, now: now).on,
                     "A pending revoke must keep showing actual access until the worker observes it revoked")
        flow.cancel(.accessibility)
        flow.cancel(.screenRecording)

        // Pending survives the gap during a restart and settles honestly on
        // the new process, for either permission. No OS access is requested.
        flow.begin(.accessibility, on: true, pid: 100)
        precondition(flow.row(.accessibility, nil, restarting: true, now: now).pending)
        precondition(flow.observe(report(pid: 101, ax: "Not granted"), now: now))
        precondition(!flow.row(.accessibility, report(pid: 101, ax: "Not granted"), restarting: false, now: now).pending)
        let toggle = MemQLSwitch()
        toggle.isPending = true
        precondition((toggle.accessibilityValue() as? NSNumber)?.intValue == 2,
                     "VoiceOver distinguishes pending from off")
        var requested: Bool?
        toggle.onToggle = { requested = $0 }
        precondition(toggle.accessibilityPerformPress() && requested == false,
                     "Pressing a pending switch cancels instead of issuing another prompt")
        toggle.isPending = false
        precondition(toggle.accessibilityPerformPress() && requested == true)
        precondition((toggle.accessibilityValue() as? NSNumber)?.intValue == 1)

        // Setup waits on this window (memql-cockpit#548): the way on without
        // what is not allowed yet, offered only while it waits.
        func waiting(ax: String, screen: String) -> PermissionReport {
            var waiting = report(ax: ax, screen: screen); waiting.setupWaiting = true; return waiting
        }
        precondition(flow.setupChoice(neither, restarting: false, now: now) == nil, "No setup waiting, no choice")
        precondition(flow.setupChoice(waiting(ax: "Not granted", screen: "Not granted"), restarting: false, now: now) == "Skip for now")
        precondition(flow.setupChoice(waiting(ax: "Granted", screen: "Not granted"), restarting: false, now: now) == "Continue without Screen Recording")
        precondition(flow.setupChoice(waiting(ax: "Not granted", screen: "Granted"), restarting: false, now: now) == "Continue without Accessibility")
        precondition(flow.setupChoice(waiting(ax: "Granted", screen: "Granted"), restarting: false, now: now) == nil, "Both allowed: setup goes on by itself")
        precondition(flow.setupChoice(waiting(ax: "Not granted", screen: "Not granted"), restarting: true, now: now) == nil, "Nothing is chosen while MemQL restarts")
        var stale = waiting(ax: "Not granted", screen: "Not granted"); stale = PermissionReport(pid: stale.pid, version: stale.version, executable: stale.executable,
            accessibility: stale.accessibility, screenRecording: stale.screenRecording, requestsSupported: true, requestPending: false,
            checkedAt: now.addingTimeInterval(-16), setupWaiting: true)
        precondition(flow.setupChoice(stale, restarting: false, now: now) == nil, "No choice without a fresh report")
        let decoder = JSONDecoder()
        let running = try! decoder.decode(SetupStatus.self, from: Data(#"{"state":"running","stages":[{"name":"permissions","state":"running","detail":""},{"name":"models","state":"waiting","completed_bytes":0}]}"#.utf8))
        precondition(running.waitingForPermissions)
        let answered = try! decoder.decode(SetupStatus.self, from: Data(#"{"state":"running","stages":[{"name":"permissions","state":"running"}],"answers":{"permissions":{"stage":"permissions","choice":"skip"}}}"#.utf8))
        precondition(!answered.waitingForPermissions, "An answered wait is no longer waiting")
        let downloading = try! decoder.decode(SetupStatus.self, from: Data(#"{"state":"running","stages":[{"name":"permissions","state":"skipped"},{"name":"models","state":"running"}]}"#.utf8))
        precondition(!downloading.waitingForPermissions, "Setup past the permissions waits on nobody here")

        let service = "service = {\n\tprogram = /fixture/memql\n\tpid = 100\n}"
        precondition(PermissionBridge.serviceMatches(service, pid: 100, executable: "/fixture/memql"))
        precondition(!PermissionBridge.serviceMatches(service, pid: 10, executable: "/fixture/memql"))
        precondition(!PermissionBridge.serviceMatches(service, pid: 100, executable: "/other/memql"))
        if let output = CommandLine.arguments.first(where: { $0.hasPrefix("--render=") }) {
            // Offscreen fixture only: no worker connection, OS permission
            // request, deep-link registration or service action.
            let app = NSApplication.shared
            app.setActivationPolicy(.prohibited)
            let setup = PermissionSetupWindow(logoImage: MarkParser.image(size: 48, template: false,
                resourceURL: URL(fileURLWithPath: FileManager.default.currentDirectoryPath).appendingPathComponent("native/macos/mark.svg")))
            let state = CommandLine.arguments.first(where: { $0.hasPrefix("--state=") }).map { String($0.dropFirst("--state=".count)) } ?? "idle"
            let granted = state == "ready"
            setup.update(PermissionReport(pid: 57495, version: "preview", executable: "/Users/example/Applications/MemQL.app/Contents/MacOS/MemQL",
                accessibility: granted || state == "screen" || state == "setup-partial" ? "Granted" : "Not granted", screenRecording: granted ? "Granted" : "Not granted",
                requestsSupported: true, requestPending: false, checkedAt: Date(), setupWaiting: state.hasPrefix("setup")))
            switch state {
            case "asking": setup.preview(asks: [.accessibility: true])
            case "screen", "screen-first": setup.preview(asks: [.screenRecording: true])
            case "troubleshooting": setup.preview(asks: [:], troubleshooting: true)
            default: break
            }
            let view = setup.window!.contentView!
            let appearance = NSAppearance(named: CommandLine.arguments.contains("--dark") ? .darkAqua : .aqua)!
            view.appearance = appearance
            view.wantsLayer = true
            appearance.performAsCurrentDrawingAppearance { view.layer?.backgroundColor = NSColor.windowBackgroundColor.cgColor }
            view.layoutSubtreeIfNeeded()
            let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds)!
            appearance.performAsCurrentDrawingAppearance { view.cacheDisplay(in: view.bounds, to: bitmap) }
            try! bitmap.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: String(output.dropFirst("--render=".count))))
        }
        print("Permission setup: freshness, denials, restart/new PID, timeout, URL, service identity, switch flow, setup choice and single-action checks passed.")
    }
}
