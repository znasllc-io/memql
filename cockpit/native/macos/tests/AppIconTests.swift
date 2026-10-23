import AppKit

// Export the actual icon in both appearances and at Settings-scale sizes.
// This helper renders files only; it never registers or starts an app/service.
@main
struct AppIconTests {
    static func bitmap(_ image: NSImage, pixels: Int, appearance: NSAppearance) -> NSBitmapImageRep {
        let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels,
            bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
            colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: bitmap)
        appearance.performAsCurrentDrawingAppearance {
            image.draw(in: NSRect(x: 0, y: 0, width: pixels, height: pixels))
        }
        NSGraphicsContext.restoreGraphicsState()
        return bitmap
    }
    static func main() throws {
        let resource = CommandLine.arguments.first(where: { $0.hasPrefix("--resource=") }).map { URL(fileURLWithPath: String($0.dropFirst("--resource=".count))) }
            ?? URL(fileURLWithPath: FileManager.default.currentDirectoryPath).appendingPathComponent("native/macos/mark.svg")
        let light = NSAppearance(named: .aqua)!, dark = NSAppearance(named: .darkAqua)!
        for size in [16, 24, 32, 64, 128, 256, 512] {
            let icon = MemQLAppIcon.image(pixels: size, points: size, resourceURL: resource)!
            let a = bitmap(icon, pixels: size, appearance: light)
            let b = bitmap(icon, pixels: size, appearance: dark)
            precondition(a.representation(using: .png, properties: [:]) == b.representation(using: .png, properties: [:]), "Static app icon changed with appearance")
            var white = 0, green = 0
            for y in 0..<size { for x in 0..<size {
                let c = a.colorAt(x: x, y: y)!.usingColorSpace(.sRGB)!
                if c.alphaComponent > 0.95 && c.redComponent > 0.7 && c.greenComponent > 0.8 { white += 1 }
                if c.alphaComponent > 0.95 && c.greenComponent > 0.3 && c.redComponent < 0.1 { green += 1 }
            } }
            precondition(white >= max(4, size * size / 100), "Mark is not legible at small size")
            precondition(green > size * size / 3, "Opaque contrasting tile is missing")
        }
        if let arg = CommandLine.arguments.first(where: { $0.hasPrefix("--render=") }) {
            let width = 840, height = 400
            let sheet = NSImage(size: NSSize(width: width, height: height), flipped: true) { _ in
                for (row, background) in [NSColor(srgbRed: 0.96, green: 0.96, blue: 0.97, alpha: 1), NSColor(srgbRed: 0.13, green: 0.13, blue: 0.14, alpha: 1)].enumerated() {
                    background.setFill()
                    NSRect(x: 0, y: row * 200, width: width, height: 200).fill()
                    let textColor: NSColor = row == 0 ? .black : .white
                    let labelStyle: [NSAttributedString.Key: Any] = [.font: NSFont.systemFont(ofSize: 13), .foregroundColor: textColor]
                    (row == 0 ? "Light Settings background" : "Dark Settings background").draw(at: NSPoint(x: 24, y: row * 200 + 15), withAttributes: labelStyle)
                    for (column, points) in [16, 24, 32, 64, 128].enumerated() {
                        let x = 24 + column * 160
                        let icon = MemQLAppIcon.image(pixels: points * 2, points: points, resourceURL: resource)!
                        icon.draw(in: NSRect(x: x, y: row * 200 + 48, width: points, height: points))
                        "\(points) pt".draw(at: NSPoint(x: x, y: row * 200 + 178), withAttributes: labelStyle)
                    }
                }
                return true
            }
            // Render at 2x for native Retina inspection, without scaling the icons.
            let out = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: width * 2, pixelsHigh: height * 2,
                bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
            NSGraphicsContext.saveGraphicsState()
            NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: out)
            sheet.draw(in: NSRect(x: 0, y: 0, width: width * 2, height: height * 2))
            NSGraphicsContext.restoreGraphicsState()
            try out.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: String(arg.dropFirst("--render=".count))))
        }
        print("App icon: opaque green tile and white mark remain visible at 16–512 px; static export is identical in light and dark appearances.")
    }
}
