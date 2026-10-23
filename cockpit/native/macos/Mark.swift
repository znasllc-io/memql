import AppKit

// Read the canonical MemQL SVG geometry. The original mark is bundled,
// rather than approximating it with a system glyph or redrawing a variant.
final class MarkParser: NSObject, XMLParserDelegate {
    var edges: [(CGFloat, CGFloat, CGFloat, CGFloat)] = []
    var nodes: [(CGFloat, CGFloat, CGFloat)] = []
    func parser(_ parser: XMLParser, didStartElement name: String, namespaceURI: String?, qualifiedName: String?, attributes: [String: String]) {
        func n(_ key: String) -> CGFloat { CGFloat(Double(attributes[key] ?? "0") ?? 0) }
        if name == "line" { edges.append((n("x1"), n("y1"), n("x2"), n("y2"))) }
        if name == "circle" { nodes.append((n("cx"), n("cy"), n("r"))) }
    }
    static func image(size: CGFloat = 20, template: Bool = true, resourceURL: URL? = nil, color: NSColor? = nil, strokeWidth: CGFloat = 0.78) -> NSImage? {
        guard let url = resourceURL ?? Bundle.main.url(forResource: "mark", withExtension: "svg"),
              let parser = XMLParser(contentsOf: url) else { return nil }
        let shape = MarkParser()
        parser.delegate = shape
        guard parser.parse(), shape.nodes.count == 9 else { return nil }
        let image = NSImage(size: NSSize(width: size, height: size), flipped: true) { rect in
            let accent = NSColor(name: nil) { appearance in
                appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
                    ? NSColor(srgbRed: 92/255, green: 205/255, blue: 167/255, alpha: 1)
                    : NSColor(srgbRed: 4/255, green: 125/255, blue: 90/255, alpha: 1)
            }
            (color ?? (template ? NSColor.black : accent)).set()
            let scale = rect.width / 24
            let lines = NSBezierPath()
            lines.lineWidth = strokeWidth * scale
            lines.lineCapStyle = .round
            for (x1, y1, x2, y2) in shape.edges {
                lines.move(to: NSPoint(x: x1 * scale, y: y1 * scale))
                lines.line(to: NSPoint(x: x2 * scale, y: y2 * scale))
            }
            lines.stroke()
            for (x, y, r) in shape.nodes {
                NSBezierPath(ovalIn: NSRect(x: (x-r)*scale, y: (y-r)*scale, width: 2*r*scale, height: 2*r*scale)).fill()
            }
            return true
        }
        image.isTemplate = template
        return image
    }
}

// App icons are static raster assets. Never bake the exporting Mac's current
// appearance into a transparent mark: Settings can place it on a pale tile in
// either theme. Keep a fixed opaque ground and white canonical geometry.
enum MemQLAppIcon {
    static let background = NSColor(srgbRed: 0, green: 102/255, blue: 68/255, alpha: 1)
    static func image(pixels: Int, points: Int, resourceURL: URL? = nil) -> NSImage? {
        let size = CGFloat(pixels)
        guard let mark = MarkParser.image(size: size, template: false, resourceURL: resourceURL,
                                         color: .white, strokeWidth: points <= 32 ? 1.02 : 0.78) else { return nil }
        return NSImage(size: NSSize(width: size, height: size), flipped: true) { rect in
            let tile = rect.insetBy(dx: size * 0.035, dy: size * 0.035)
            background.setFill()
            NSBezierPath(roundedRect: tile, xRadius: size * 0.205, yRadius: size * 0.205).fill()
            mark.draw(in: tile.insetBy(dx: size * 0.035, dy: size * 0.035))
            return true
        }
    }
}
