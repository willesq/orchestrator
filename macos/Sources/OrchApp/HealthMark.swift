import AppKit
import OrchKit
import SwiftUI

/// The app icon while orchd is healthy; otherwise the health symbol in its
/// warning color, which should catch the eye rather than blend in as branding.
struct HealthMark: View {
    let health: Health
    var size: CGFloat = 28

    var body: some View {
        if case .healthy = health {
            Image(nsImage: NSApp.applicationIconImage)
                .resizable()
                .frame(width: size, height: size)
                .accessibilityLabel("Orch")
        } else {
            Image(systemName: health.symbolName)
                .font(.system(size: size * 0.75))
                .foregroundStyle(tint)
                .frame(width: size, height: size)
        }
    }

    private var tint: Color {
        switch health {
        case .degraded: .orange
        case .down: .red
        case .healthy, .unknown: .secondary
        }
    }
}

enum AboutPanel {
    /// The standard About panel, which takes the icon and version from the
    /// bundle. A menu-bar-only app has no app menu to reach it from.
    @MainActor
    static func show() {
        NSApp.orderFrontStandardAboutPanel(options: [
            .applicationName: "Orch",
            .credits: NSAttributedString(
                string: "Local-first memory and task orchestration for coding agents.",
                attributes: [.font: NSFont.systemFont(ofSize: NSFont.smallSystemFontSize),
                             .foregroundColor: NSColor.secondaryLabelColor]
            ),
        ])
        NSApp.activate()
    }
}
