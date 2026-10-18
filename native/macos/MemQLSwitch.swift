import AppKit

// The MemQL brand colours this app draws with, from MemQL OS's own tokens
// (clients/os/src/styles/tokens.css in the engine repository): the accent, the
// muted ink, and the solid surface a switch's knob is cut from. Light and dark
// follow the Mac's appearance, as the OS follows its theme.
enum MemQLColor {
    static let accent = dynamic(dark: 0x5CCDA7, light: 0x047D5A)
    static let muted = dynamic(dark: 0x9CA395, light: 0x5B615C)
    static let surface = dynamic(dark: 0x0E1311, light: 0xFBFCFA)

    private static func dynamic(dark: Int, light: Int) -> NSColor {
        NSColor(name: nil) { appearance in
            appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? rgb(dark) : rgb(light)
        }
    }
    private static func rgb(_ hex: Int) -> NSColor {
        NSColor(srgbRed: CGFloat((hex >> 16) & 0xFF) / 255, green: CGFloat((hex >> 8) & 0xFF) / 255,
                blue: CGFloat(hex & 0xFF) / 255, alpha: 1)
    }
}

// MemQL OS's switch (kit/Switch.tsx, `.os-switch`), drawn natively so the
// Cockpit asks the same way the OS does: a 32 x 20 track, a 14-point knob
// inset by 3, the accent when on, the muted ink when off, and the whole
// control at 55% when it cannot be used. NSSwitch would draw in the Mac's
// accent colour instead, which is why this is not one.
//
// It is a real control: Space toggles it, it takes keyboard focus with a ring
// around the track, and VoiceOver reads it as a switch with its label.
final class MemQLSwitch: NSControl {
    /// Called when the PERSON toggles it, with the new position. Setting
    /// `isOn` from code never calls it.
    var onToggle: ((Bool) -> Void)?

    var isOn = false {
        didSet {
            guard oldValue != isOn else { return }
            needsDisplay = true
            NSAccessibility.post(element: self, notification: .valueChanged)
        }
    }

    override var isEnabled: Bool { didSet { needsDisplay = true } }
    override var intrinsicContentSize: NSSize { NSSize(width: 32, height: 20) }
    override var acceptsFirstResponder: Bool { isEnabled }
    override var focusRingMaskBounds: NSRect { track }

    private var track: NSRect { NSRect(x: (bounds.width - 32) / 2, y: (bounds.height - 20) / 2, width: 32, height: 20) }

    override func draw(_ dirtyRect: NSRect) {
        let alpha: CGFloat = isEnabled ? 1 : 0.55
        (isOn ? MemQLColor.accent : MemQLColor.muted).withAlphaComponent(alpha).setFill()
        NSBezierPath(roundedRect: track, xRadius: 10, yRadius: 10).fill()
        let knob = NSRect(x: track.minX + 3 + (isOn ? 12 : 0), y: track.minY + 3, width: 14, height: 14)
        MemQLColor.surface.withAlphaComponent(alpha).setFill()
        NSBezierPath(ovalIn: knob).fill()
    }

    override func drawFocusRingMask() {
        NSBezierPath(roundedRect: track, xRadius: 10, yRadius: 10).fill()
    }

    override func mouseDown(with event: NSEvent) {
        guard isEnabled else { return }
        toggle()
    }

    override func keyDown(with event: NSEvent) {
        if isEnabled, event.charactersIgnoringModifiers == " " { toggle() } else { super.keyDown(with: event) }
    }

    private func toggle() {
        isOn.toggle()
        onToggle?(isOn)
    }

    override func isAccessibilityElement() -> Bool { true }
    override func accessibilityRole() -> NSAccessibility.Role? { .checkBox }
    override func accessibilitySubrole() -> NSAccessibility.Subrole? { .switch }
    override func accessibilityValue() -> Any? { NSNumber(value: isOn) }
    override func isAccessibilityEnabled() -> Bool { isEnabled }
    override func accessibilityPerformPress() -> Bool {
        guard isEnabled else { return false }
        toggle()
        return true
    }
}
