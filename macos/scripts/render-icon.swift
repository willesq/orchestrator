// Renders an SVG into a macOS .iconset (every size iconutil expects), with
// AppKit's own SVG support, so building the icon needs no extra tools.
//
//   swift render-icon.swift <icon.svg> <out.iconset>
import AppKit

let args = CommandLine.arguments
guard args.count == 3, let image = NSImage(contentsOfFile: args[1]) else {
    FileHandle.standardError.write(Data("usage: render-icon <icon.svg> <out.iconset> (could not load the SVG)\n".utf8))
    exit(2)
}
let out = URL(fileURLWithPath: args[2])
try FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)

for points in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let pixels = points * scale
        guard let rep = NSBitmapImageRep(
            bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels, bitsPerSample: 8,
            samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
            bytesPerRow: 0, bitsPerPixel: 0
        ) else { exit(1) }
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
        NSGraphicsContext.current?.imageInterpolation = .high
        image.draw(in: NSRect(x: 0, y: 0, width: pixels, height: pixels))
        NSGraphicsContext.restoreGraphicsState()
        let name = scale == 1 ? "icon_\(points)x\(points).png" : "icon_\(points)x\(points)@2x.png"
        guard let png = rep.representation(using: .png, properties: [:]) else {
            FileHandle.standardError.write(Data("render-icon: could not encode \(name)\n".utf8))
            exit(1)
        }
        try png.write(to: out.appendingPathComponent(name))
    }
}
