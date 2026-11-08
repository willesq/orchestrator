import OrchKit
import SwiftUI

struct LogsView: View {
    @Bindable var model: LogsModel

    var body: some View {
        // Filtered once per update, not once per use.
        let visible = model.visibleLines
        VStack(spacing: 0) {
            controls(count: visible.count)
                .padding(.horizontal, 12)
                .padding(.vertical, 8)
            Divider()
            if model.missed > 0 {
                Label("\(model.missed) earlier lines rolled out of orchd's memory before they could be shown",
                      systemImage: "exclamationmark.triangle")
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .padding(6)
            }
            if let error = model.lastError {
                ErrorText("orchd: \(error)", symbol: "bolt.slash")
                    .padding(6)
            }
            LogLines(lines: visible, follow: !model.paused)
        }
    }

    private func controls(count: Int) -> some View {
        HStack(spacing: 10) {
            Picker("Source", selection: $model.source) {
                Text("All sources").tag(String?.none)
                Divider()
                ForEach(model.sources, id: \.self) { Text(verbatim: $0).tag(String?.some($0)) }
            }
            .fixedSize()
            TextField("Filter", text: $model.filter)
                .textFieldStyle(.roundedBorder)
                .frame(maxWidth: 260)
            Spacer()
            Text("\(count) line\(count == 1 ? "" : "s")")
                .textStyle(.caption)
                .monospacedDigit()
            Toggle(isOn: $model.paused) {
                Label(model.paused ? "Resume" : "Pause", systemImage: model.paused ? "play.fill" : "pause.fill")
            }
            .toggleStyle(.button)
            .help(model.paused ? "Fetch new lines again" : "Stop fetching new lines")
            .accessibilityLabel(model.paused ? "Resume" : "Pause")
            Button { model.clear() } label: { Label("Clear", systemImage: "trash") }
                .help("Clear the lines shown; new ones keep arriving")
                .accessibilityLabel("Clear")
        }
    }
}

private struct LogLines: View {
    let lines: [LogLine]
    let follow: Bool

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 1) {
                    ForEach(lines) { LogRow(line: $0) }
                }
                .padding(8)
                .textSelection(.enabled)
            }
            .background(Color(nsColor: .textBackgroundColor))
            .onChange(of: lines.last?.seq) { _, last in
                guard follow, let last else { return }
                proxy.scrollTo(last, anchor: .bottom)
            }
            .onAppear {
                if let last = lines.last?.seq { proxy.scrollTo(last, anchor: .bottom) }
            }
            .overlay {
                if lines.isEmpty {
                    EmptyState(symbol: "text.alignleft", title: "No log lines yet",
                               message: "Lines appear here as orchd and its subsystems log them.")
                }
            }
        }
    }
}

private struct LogRow: View {
    let line: LogLine

    private static let time: DateFormatter = {
        let f = DateFormatter()
        f.dateFormat = "HH:mm:ss.SSS"
        return f
    }()

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(verbatim: Self.time.string(from: line.at))
                .foregroundStyle(Color.tertiaryText)
            Text(verbatim: line.source)
                .foregroundStyle(SourceColor.of(line.source))
                .frame(width: 104, alignment: .leading)
                .lineLimit(1)
            // Log text is untrusted (qdrant's output, other people's PR
            // titles), so it is shown verbatim.
            Text(verbatim: line.message)
                .foregroundStyle(isError ? Color.red : Color.primary)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .font(.system(size: 11, design: .monospaced))
        .id(line.seq)
    }

    private var isError: Bool {
        let m = line.message.lowercased()
        return m.contains("error") || m.contains("failed") || m.contains("panic")
    }
}

/// A stable color per source, so a source reads the same in every row.
enum SourceColor {
    static let palette: [Color] = [.blue, .purple, .teal, .orange, .pink, .indigo, .brown, .mint, .cyan]

    static func of(_ source: String) -> Color {
        if source == "orchd" { return .secondaryText }
        let hash = source.unicodeScalars.reduce(UInt32(5381)) { ($0 &* 33) &+ $1.value }
        return palette[Int(hash % UInt32(palette.count))]
    }
}
