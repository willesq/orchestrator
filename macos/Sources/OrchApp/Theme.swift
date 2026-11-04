import AppKit
import OrchKit
import SwiftUI

// The app's shared look: one type scale, readable secondary text, one
// raised card surface, chips, status dots and empty states. Material's ideas
// (hierarchy, tonal surfaces, state colors, an obvious primary action) built
// from system colors and fonts, so both appearances and Dynamic Type follow
// the system.

/// The text styles every tab uses. Monospace is only for what a person
/// might type or copy: commands, paths, code, rule tokens.
enum TextStyle {
    /// The heading of a pane's main object.
    case title
    /// A card or section heading.
    case section
    /// A list row's main line.
    case rowTitle
    /// Supporting text under a row title or in a card.
    case supporting
    /// Small labels, timestamps, counts.
    case caption
    /// A command, path, rule or other literal.
    case code
    /// A small literal under a row title.
    case codeCaption

    var font: Font {
        switch self {
        case .title: .title3.weight(.semibold)
        case .section: .headline
        case .rowTitle: .body
        case .supporting: .callout
        case .caption: .caption
        case .code: .callout.monospaced()
        case .codeCaption: .caption.monospaced()
        }
    }

    var color: Color {
        switch self {
        case .title, .section, .rowTitle, .code: .primary
        case .supporting, .caption, .codeCaption: .secondaryText
        }
    }
}

extension View {
    func textStyle(_ style: TextStyle) -> some View {
        font(style.font).foregroundStyle(style.color)
    }
}

extension Color {
    /// Secondary text with WCAG AA contrast on the window and card surfaces
    /// in both appearances; the system's secondary label is lighter.
    static let secondaryText = Color(nsColor: NSColor(name: nil) { appearance in
        appearance.isDark ? NSColor(white: 1, alpha: 0.72) : NSColor(white: 0, alpha: 0.68)
    })

    /// For the least important text that must still be read (timestamps).
    static let tertiaryText = Color(nsColor: NSColor(name: nil) { appearance in
        appearance.isDark ? NSColor(white: 1, alpha: 0.58) : NSColor(white: 0, alpha: 0.6)
    })

    /// A card one tonal step off the content background: lighter in dark
    /// mode, a faint grey on the white content area in light mode.
    static let raisedSurface = Color(nsColor: NSColor(name: nil) { appearance in
        appearance.isDark ? NSColor(white: 1, alpha: 0.055) : NSColor(white: 0, alpha: 0.035)
    })

    /// The hairline around a raised card.
    static let surfaceStroke = Color(nsColor: NSColor(name: nil) { appearance in
        appearance.isDark ? NSColor(white: 1, alpha: 0.08) : NSColor(white: 0, alpha: 0.08)
    })
}

extension NSAppearance {
    var isDark: Bool { bestMatch(from: [.darkAqua, .aqua]) == .darkAqua }
}

/// Spacing and shape shared by every card.
enum Metrics {
    static let cardRadius: CGFloat = 10
    static let cardPadding: CGFloat = 14
    static let pagePadding: CGFloat = 20
    static let stackSpacing: CGFloat = 16
}

/// A raised content card with an optional heading and trailing accessory.
struct Card<Content: View, Accessory: View>: View {
    var title: String?
    @ViewBuilder var accessory: () -> Accessory
    @ViewBuilder var content: () -> Content

    init(_ title: String? = nil, @ViewBuilder accessory: @escaping () -> Accessory,
         @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.accessory = accessory
        self.content = content
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if title != nil || Accessory.self != EmptyView.self {
                HStack(alignment: .firstTextBaseline) {
                    if let title { Text(title).textStyle(.section).accessibilityAddTraits(.isHeader) }
                    Spacer(minLength: 8)
                    accessory()
                }
            }
            content()
        }
        .padding(Metrics.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .cardSurface()
    }
}

extension Card where Accessory == EmptyView {
    init(_ title: String? = nil, @ViewBuilder content: @escaping () -> Content) {
        self.init(title, accessory: { EmptyView() }, content: content)
    }
}

extension View {
    /// The raised card surface, for content that is not a `Card`.
    func cardSurface(radius: CGFloat = Metrics.cardRadius) -> some View {
        background(Color.raisedSurface, in: RoundedRectangle(cornerRadius: radius, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: radius, style: .continuous).strokeBorder(Color.surfaceStroke))
    }
}

/// The state colors chips and dots use.
enum Tone: Equatable {
    case neutral, accent, good, warning, bad

    var color: Color {
        switch self {
        case .neutral: .secondaryText
        case .accent: .accentColor
        case .good: .green
        case .warning: .orange
        case .bad: .red
        }
    }
}

/// A small labelled capsule: a status, an origin, a reason.
struct Chip: View {
    let text: String
    var tone: Tone = .neutral
    var symbol: String?
    var help: String?

    var body: some View {
        HStack(spacing: 3) {
            if let symbol { Image(systemName: symbol).imageScale(.small) }
            Text(text)
        }
        .font(.caption.weight(.medium))
        .padding(.horizontal, 7)
        .padding(.vertical, 2)
        .background(tone.color.opacity(tone == .neutral ? 0.14 : 0.18), in: Capsule())
        .foregroundStyle(tone.color)
        .help(help ?? "")
        .accessibilityElement(children: .combine)
    }
}

/// A colored dot for a state, with its meaning for VoiceOver.
struct StatusDot: View {
    let tone: Tone
    var label: String
    var size: CGFloat = 8

    var body: some View {
        Circle()
            .fill(tone.color)
            .frame(width: size, height: size)
            .accessibilityLabel(label)
    }
}

/// An empty pane: what is missing, why, and the action that fills it.
struct EmptyState: View {
    let symbol: String
    let title: String
    var message: String?
    var actionTitle: String?
    var action: (() -> Void)?

    var body: some View {
        VStack(spacing: 10) {
            Image(systemName: symbol)
                .font(.system(size: 34, weight: .light))
                .foregroundStyle(Color.secondaryText)
                .accessibilityHidden(true)
            Text(title).font(.title3.weight(.semibold))
            if let message {
                Text(message)
                    .textStyle(.supporting)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: 360)
            }
            if let actionTitle, let action {
                Button(actionTitle, action: action)
                    .buttonStyle(.borderedProminent)
                    .controlSize(.large)
                    .padding(.top, 4)
            }
        }
        .padding(24)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

/// Light, Dark or the system's choice. Shown in the menu bar panel and at
/// the foot of the sidebar; both write the same setting.
struct AppearancePicker: View {
    @AppStorage(AppearanceChoice.key) private var choice: AppearanceChoice = .system

    var body: some View {
        Picker("Appearance", selection: $choice) {
            ForEach(AppearanceChoice.allCases) { Text($0.label).tag($0) }
        }
        .help("Follow the system's light or dark mode, or keep one")
        .onChange(of: choice) { _, new in new.apply() }
    }
}

/// Shows or hides the Dock icon at once, without a relaunch.
struct DockIconToggle: View {
    @AppStorage(DockIcon.key) private var shown = true

    var body: some View {
        Toggle("Show in Dock", isOn: $shown)
            .help("Keep an icon in the Dock as well as the menu bar")
            .onChange(of: shown) { _, new in
                DockIcon.apply(shown: new)
                // Changing the policy can push the app's windows behind others.
                NSApp.activate()
            }
    }
}
