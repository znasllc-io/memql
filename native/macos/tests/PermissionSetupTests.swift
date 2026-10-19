import Foundation
import AppKit

@main
enum PermissionSetupTests {
    static func main() {
        let now = Date()
        func report(pid: Int = 100, ax: String = "Granted", screen: String = "Granted", age: TimeInterval = 0) -> PermissionReport {
            PermissionReport(pid: pid, version: "fixture", executable: "/fixture/memql", accessibility: ax,
                             screenRecording: screen, requestsSupported: true, requestPending: false,
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
        let service = "service = {\n\tprogram = /fixture/memql\n\tpid = 100\n}"
        precondition(PermissionBridge.serviceMatches(service, pid: 100, executable: "/fixture/memql"))
        precondition(!PermissionBridge.serviceMatches(service, pid: 10, executable: "/fixture/memql"))
        precondition(!PermissionBridge.serviceMatches(service, pid: 100, executable: "/other/memql"))
        // Registered by MemQL.app through SMAppService, launchd names the program inside the bundle.
        let bundled = "service = {\n\tprogram identifier = Contents/MacOS/MemQL (mode: 2)\n\tparent bundle identifier = com.znasllc.memql-worker\n\tpid = 100\n}"
        precondition(PermissionBridge.serviceMatches(bundled, pid: 100, executable: "/Users/example/Applications/MemQL.app/Contents/MacOS/MemQL"))
        precondition(!PermissionBridge.serviceMatches(bundled, pid: 101, executable: "/Users/example/Applications/MemQL.app/Contents/MacOS/MemQL"))
        precondition(!PermissionBridge.serviceMatches(bundled, pid: 100, executable: "/Users/example/.memql/bin/memql"), "A raw binary is never the bundle's program")
        precondition(!PermissionBridge.serviceMatches(bundled.replacingOccurrences(of: "com.znasllc.memql-worker", with: "com.example.other"), pid: 100,
                                                      executable: "/Users/example/Applications/MemQL.app/Contents/MacOS/MemQL"))
        if let output = CommandLine.arguments.first(where: { $0.hasPrefix("--render=") }) {
            // Offscreen fixture only: no worker connection, OS permission
            // request, deep-link registration or service action.
            let app = NSApplication.shared
            app.setActivationPolicy(.prohibited)
            let setup = PermissionSetupWindow(logoImage: MarkParser.image(size: 48, template: false,
                resourceURL: URL(fileURLWithPath: FileManager.default.currentDirectoryPath).appendingPathComponent("native/macos/mark.svg")))
            setup.update(PermissionReport(pid: 57495, version: "preview", executable: "/Users/example/.memql/bin/memql-computeruse-darwin-arm64",
                accessibility: "Not granted", screenRecording: "Not granted", requestsSupported: true, requestPending: false, checkedAt: Date()))
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
        print("Permission setup: freshness, denials, restart/new PID, timeout, URL and service identity checks passed.")
    }
}
