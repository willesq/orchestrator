import OrchKit
import SwiftUI

/// One thing the palette can do: go somewhere, open something, or act.
struct PaletteCommand: Identifiable {
    let id: String
    let section: String
    let title: String
    var subtitle: String?
    let symbol: String
    var tone: Tone = .neutral
    let run: () -> Void

    var searchText: String { [title, subtitle, section].compactMap(\.self).joined(separator: " ") }
}

/// The ⌘K palette: one keyboard surface over every pane, the PRs that need
/// the user, recent tasks and the app's actions, so steering orch never needs
/// the mouse. It only navigates inside the app and starts what the
/// panes already offer; anything destructive stays behind its pane's confirmation.
struct CommandPaletteView: View {
    let commands: [PaletteCommand]
    let dismiss: () -> Void

    @State private var query = ""
    @State private var highlighted = 0
    @FocusState private var focused: Bool

    private var results: [PaletteCommand] {
        PaletteMatcher.grouped(PaletteMatcher.rank(commands, query: query, limit: 40) { $0.searchText }) { $0.section }
    }

    var body: some View {
        let results = results
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                Image(systemName: "command")
                    .font(.title3.weight(.semibold))
                    .foregroundStyle(.secondary)
                TextField("Jump to a pane, PR or task, or run an action", text: $query)
                    .textFieldStyle(.plain)
                    .font(.title3)
                    .focused($focused)
                    .onSubmit { run(results) }
                Text("esc")
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 6)
                    .padding(.vertical, 2)
                    .background(.quaternary, in: RoundedRectangle(cornerRadius: 4))
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 14)

            Divider()

            // A fixed height, so the panel hosting it never resizes as results change.
            Group {
            if results.isEmpty {
                Text("Nothing matches “\(query)”")
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollViewReader { proxy in
                    ScrollView {
                        LazyVStack(alignment: .leading, spacing: 2) {
                            ForEach(Array(results.enumerated()), id: \.element.id) { index, command in
                                if index == 0 || results[index - 1].section != command.section {
                                    Text(command.section.uppercased())
                                        .font(.caption2.weight(.semibold))
                                        .foregroundStyle(.secondary)
                                        .padding(.horizontal, 10)
                                        .padding(.top, index == 0 ? 4 : 10)
                                }
                                PaletteRow(command: command, highlighted: index == highlighted)
                                    .id(command.id)
                                    .onTapGesture { command.run(); dismiss() }
                                    .onHover { if $0 { highlighted = index } }
                            }
                        }
                        .padding(8)
                    }
                    .onChange(of: highlighted) { _, new in
                        guard results.indices.contains(new) else { return }
                        proxy.scrollTo(results[new].id)
                    }
                }
            }
            }
            .frame(height: 380)

            Divider()
            HStack(spacing: 14) {
                hint("↑↓", "move")
                hint("↩", "run")
                hint("⌘K", "toggle")
                Spacer()
                Text("\(results.count) of \(commands.count)")
                    .monospacedDigit()
            }
            .font(.caption)
            .foregroundStyle(.secondary)
            .padding(.horizontal, 16)
            .padding(.vertical, 8)
        }
        .frame(width: 600)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 14))
        .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(.separator))
        .onAppear { focused = true }
        .onChange(of: query) { highlighted = 0 }
        .onKeyPress(.downArrow) { move(1, in: results); return .handled }
        .onKeyPress(.upArrow) { move(-1, in: results); return .handled }
        .onKeyPress(.escape) { dismiss(); return .handled }
        .onKeyPress(keys: ["k"]) { press in
            guard press.modifiers.contains(.command) else { return .ignored }
            dismiss()
            return .handled
        }
    }

    private func move(_ step: Int, in results: [PaletteCommand]) {
        guard !results.isEmpty else { return }
        highlighted = (highlighted + step + results.count) % results.count
    }

    private func run(_ results: [PaletteCommand]) {
        guard results.indices.contains(highlighted) else { return }
        results[highlighted].run()
        dismiss()
    }

    private func hint(_ key: String, _ label: String) -> some View {
        HStack(spacing: 4) {
            Text(key).font(.caption.monospaced().weight(.semibold))
            Text(label)
        }
    }
}

private struct PaletteRow: View {
    let command: PaletteCommand
    let highlighted: Bool

    var body: some View {
        HStack(spacing: 10) {
            Image(systemName: command.symbol)
                .frame(width: 20)
                .foregroundStyle(highlighted ? Color.white : command.tone.color)
            VStack(alignment: .leading, spacing: 1) {
                Text(command.title).lineLimit(1)
                if let subtitle = command.subtitle {
                    Text(subtitle)
                        .font(.caption)
                        .lineLimit(1)
                        .foregroundStyle(highlighted ? Color.white.opacity(0.85) : .secondary)
                }
            }
            Spacer(minLength: 0)
            if highlighted {
                Image(systemName: "return").font(.caption)
            }
        }
        .foregroundStyle(highlighted ? Color.white : .primary)
        .padding(.horizontal, 10)
        .padding(.vertical, 6)
        .background(highlighted ? Color.accentColor : .clear, in: RoundedRectangle(cornerRadius: 7))
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.isButton)
    }
}
