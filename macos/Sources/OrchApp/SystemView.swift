import Charts
import OrchKit
import SwiftUI

enum SystemTab: String, CaseIterable, Identifiable {
    case overview = "Overview", logs = "Logs"
    var id: String { rawValue }
}

struct SystemView: View {
    let model: SystemModel
    let logs: LogsModel
    let restart: RestartModel

    @AppStorage("systemTab") private var tab: SystemTab = .overview

    var body: some View {
        Group {
            switch tab {
            case .overview: SystemOverview(model: model, restart: restart)
            case .logs: LogsView(model: logs)
            }
        }
        .navigationTitle("System")
        .toolbar {
            ToolbarItem(placement: .principal) {
                Picker("View", selection: $tab) {
                    ForEach(SystemTab.allCases) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
            }
            ToolbarItem {
                Button {
                    Task {
                        await model.refresh()
                        await model.refreshReport()
                    }
                } label: {
                    Label("Refresh", systemImage: "arrow.clockwise")
                }
                .help("Reload metrics and the status report now")
            }
        }
        // Metrics feed both views' headers, so they poll whichever is shown.
        .onAppear {
            model.start()
            logs.start()
        }
        .onDisappear {
            model.stop()
            logs.stop()
        }
    }
}

private struct SystemOverview: View {
    let model: SystemModel
    let restart: RestartModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                if let error = model.lastError {
                    ErrorText("orchd: \(error)", symbol: "bolt.slash", font: .body)
                }
                processes
                SubsystemsBox(status: model.status, restart: restart)
                LazyVGrid(columns: Self.tileColumns, alignment: .leading, spacing: 16) {
                    StorageBox(metrics: model.metrics)
                    TasksBox(metrics: model.metrics)
                }
                ReportBox(model: model)
            }
            .padding(20)
        }
    }

    /// Tiles share a row while each gets this much room, then wrap, so a
    /// narrow window stacks them instead of squeezing their text.
    private static let tileColumns = [GridItem(.adaptive(minimum: 260), spacing: 16, alignment: .top)]

    private var processes: some View {
        LazyVGrid(columns: Self.tileColumns, alignment: .leading, spacing: 16) {
            ForEach(["orchd", "qdrant"], id: \.self) { name in
                ProcessTile(name: name, stats: model.metrics?.process(name), history: model.history[name] ?? [])
            }
            RuntimeTile(metrics: model.metrics)
        }
    }
}

private struct ProcessTile: View {
    let name: String
    let stats: ProcessStats?
    let history: [MetricSample]

    var body: some View {
        Card {
            VStack(alignment: .leading, spacing: 10) {
                HStack(alignment: .firstTextBaseline) {
                    StatusDot(tone: running ? .good : .neutral, label: running ? "running" : "not running")
                    Text(verbatim: name).textStyle(.section)
                    Spacer()
                    if let stats, stats.running {
                        Text(verbatim: "pid \(stats.pid) · up \(formatUptime(stats.uptimeSeconds))")
                            .textStyle(.caption)
                            .monospacedDigit()
                    } else {
                        Text(stats == nil ? "waiting for orchd" : "not running")
                            .textStyle(.caption)
                    }
                }
                HStack(spacing: 16) {
                    Gauge(title: "CPU", value: running ? String(format: "%.1f%%", stats?.cpuPercent ?? 0) : "–") {
                        Sparkline(samples: history, value: \.cpuPercent, color: .blue)
                    }
                    Gauge(title: "Memory", value: running ? formatBytes(stats?.rssBytes ?? 0) : "–") {
                        Sparkline(samples: history, value: { Double($0.rssBytes) }, color: .purple)
                    }
                }
            }
        }
        .frame(maxWidth: .infinity)
    }

    private var running: Bool { stats?.running == true }
}

private struct Gauge<Chart: View>: View {
    let title: String
    let value: String
    @ViewBuilder let chart: () -> Chart

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title).textStyle(.caption)
            Text(verbatim: value).font(.title3.monospacedDigit().weight(.semibold))
            chart().frame(height: 36)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

private struct Sparkline: View {
    let samples: [MetricSample]
    let value: (MetricSample) -> Double
    let color: Color

    init(samples: [MetricSample], value: @escaping (MetricSample) -> Double, color: Color) {
        self.samples = samples
        self.value = value
        self.color = color
    }

    init(samples: [MetricSample], value: KeyPath<MetricSample, Double>, color: Color) {
        self.init(samples: samples, value: { $0[keyPath: value] }, color: color)
    }

    var body: some View {
        // Scaled to the samples' own range, so a steady value is a line and
        // not a block filled up from zero.
        let domain = sparklineDomain(samples.map(value))
        Chart(samples) { s in
            AreaMark(x: .value("Time", s.at), yStart: .value("Floor", domain.lowerBound), yEnd: .value("Value", value(s)))
                .foregroundStyle(LinearGradient(colors: [color.opacity(0.28), color.opacity(0.02)], startPoint: .top, endPoint: .bottom))
                .interpolationMethod(.monotone)
            LineMark(x: .value("Time", s.at), y: .value("Value", value(s)))
                .foregroundStyle(color)
                .lineStyle(StrokeStyle(lineWidth: 1.5, lineCap: .round, lineJoin: .round))
                .interpolationMethod(.monotone)
        }
        .chartXAxis(.hidden)
        .chartYAxis(.hidden)
        .chartYScale(domain: domain)
        .chartPlotStyle { $0.clipped() }
        .overlay {
            if samples.count < 2 {
                Text("collecting…").font(.caption2).foregroundStyle(Color.tertiaryText)
            }
        }
        .accessibilityHidden(true)
    }
}

private struct RuntimeTile: View {
    let metrics: SystemMetrics?

    var body: some View {
        Card("orchd runtime") {
            Gauge(title: "Goroutines", value: metrics.map { "\($0.goroutines)" } ?? "–") { EmptyView() }
            Gauge(title: "Go heap", value: metrics.map { formatBytes($0.heapBytes) } ?? "–") { EmptyView() }
        }
        .frame(maxWidth: .infinity)
    }
}

private struct SubsystemsBox: View {
    let status: DaemonStatus?
    let restart: RestartModel

    var body: some View {
        Card("Subsystems") {
            if let status {
                Text(verbatim: "orchd \(status.version) · up \(formatUptime(status.uptimeSeconds))").textStyle(.caption)
            }
        } content: {
            if let status {
                // Ages tick every second between polls.
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    VStack(spacing: 0) {
                        ForEach(Array(status.subsystems.enumerated()), id: \.element.id) { index, unit in
                            if index > 0 { Divider().padding(.leading, 18) }
                            SubsystemRow(unit: unit, restart: restart, beat: beat(unit, now: context.date), compact: true)
                                .padding(.vertical, 5)
                        }
                    }
                }
                if let message = restart.lastMessage {
                    Text(message)
                        .font(.caption)
                        .foregroundStyle(restart.lastFailed ? Color.red : Color.secondaryText)
                }
            } else {
                Text("Waiting for orchd…").textStyle(.supporting)
            }
        }
    }

    private func beat(_ unit: Subsystem, now: Date) -> String {
        guard let last = unit.lastBeat else { return unit.isFailed ? "failed" : unit.state }
        return "beat \(formatUptime(Int64(now.timeIntervalSince(last)))) ago"
    }
}

private struct StorageBox: View {
    let metrics: SystemMetrics?

    var body: some View {
        Card("Storage") {
            VStack(alignment: .leading, spacing: 6) {
                ForEach(metrics?.data ?? []) { d in
                    HStack {
                        Text(verbatim: d.name.capitalized)
                        Spacer()
                        Text(verbatim: formatBytes(d.bytes)).monospacedDigit()
                    }
                    .help(d.path)
                }
                if metrics == nil { Text("–").textStyle(.supporting) }
            }
        }
        .frame(maxWidth: .infinity)
    }
}

private struct TasksBox: View {
    let metrics: SystemMetrics?

    var body: some View {
        Card("Tasks") {
            VStack(alignment: .leading, spacing: 6) {
                let counts = (metrics?.tasks ?? [:]).sorted { $0.key < $1.key }
                if counts.isEmpty {
                    Text(metrics == nil ? "–" : "None recorded yet").textStyle(.supporting)
                }
                ForEach(counts, id: \.key) { status, n in
                    HStack {
                        Text(verbatim: status.replacingOccurrences(of: "_", with: " ").capitalized)
                        Spacer()
                        Text(verbatim: "\(n)").monospacedDigit()
                    }
                }
            }
        }
        .frame(maxWidth: .infinity)
    }
}

private struct ReportBox: View {
    let model: SystemModel

    var body: some View {
        Card {
            VStack(alignment: .leading, spacing: 10) {
                HStack(alignment: .firstTextBaseline) {
                    Text("Status report").textStyle(.section)
                    if let report = model.report {
                        // Warnings count too: "no problems" beside a line that
                        // needs attention would contradict the report.
                        Chip(text: report.headline, tone: reportTone(report),
                             symbol: report.attentionCount == 0 ? "checkmark" : "exclamationmark.triangle.fill")
                    }
                    Spacer()
                    if model.loadingReport { ProgressView().controlSize(.small) }
                    Text("orch status, every 15 s").textStyle(.caption)
                }
                if let error = model.reportError {
                    ErrorText(error).lineLimit(4)
                }
                // Titles and names can repeat (one line per harness), so rows
                // are identified by position.
                ForEach(Array((model.report?.sections ?? []).enumerated()), id: \.offset) { _, section in
                    VStack(alignment: .leading, spacing: 4) {
                        Text(verbatim: section.title.capitalized).font(.subheadline.weight(.semibold))
                        if let note = section.note {
                            Text(verbatim: note).textStyle(.caption)
                        }
                        ForEach(Array(section.lines.enumerated()), id: \.offset) { _, line in
                            ReportLine(line: line)
                        }
                    }
                    Divider()
                }
            }
        }
    }

    private func reportTone(_ report: StatusReport) -> Tone {
        if report.sections.contains(where: { $0.lines.contains { $0.level == "bad" } }) { return .bad }
        return report.attentionCount > 0 ? .warning : .good
    }
}

private struct ReportLine: View {
    let line: StatusReport.Line

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: symbol).foregroundStyle(color).frame(width: 14)
                .accessibilityLabel(line.level ?? "info")
            Text(verbatim: line.name).frame(width: 170, alignment: .leading)
            // Paths are literals; states and versions read as plain words.
            Text(verbatim: line.value).font(line.value.contains("/") ? TextStyle.code.font : .body)
            if let detail = line.detail {
                Text(verbatim: detail).textStyle(.caption).lineLimit(2)
            }
            Spacer()
        }
        .textSelection(.enabled)
    }

    private var symbol: String {
        switch line.level {
        case "ok": "checkmark.circle.fill"
        case "warn": "exclamationmark.triangle.fill"
        case "bad": "xmark.octagon.fill"
        default: "info.circle"
        }
    }

    private var color: Color {
        switch line.level {
        case "ok": .green
        case "warn": .orange
        case "bad": .red
        default: .secondary
        }
    }
}
