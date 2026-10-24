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
                    Label("orchd: \(error)", systemImage: "bolt.slash")
                        .foregroundStyle(.red)
                }
                processes
                SubsystemsBox(status: model.status, restart: restart)
                HStack(alignment: .top, spacing: 16) {
                    StorageBox(metrics: model.metrics)
                    TasksBox(metrics: model.metrics)
                }
                ReportBox(model: model)
            }
            .padding(20)
        }
    }

    private var processes: some View {
        HStack(alignment: .top, spacing: 16) {
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
        GroupBox {
            VStack(alignment: .leading, spacing: 10) {
                HStack(alignment: .firstTextBaseline) {
                    Circle().fill(running ? Color.green : .secondary).frame(width: 8, height: 8)
                    Text(verbatim: name).font(.headline.monospaced())
                    Spacer()
                    if let stats, stats.running {
                        Text(verbatim: "pid \(stats.pid) · up \(formatUptime(stats.uptimeSeconds))")
                            .font(.caption.monospacedDigit())
                            .foregroundStyle(.secondary)
                    } else {
                        Text(stats == nil ? "waiting for orchd" : "not running")
                            .font(.caption)
                            .foregroundStyle(.secondary)
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
            .padding(4)
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
            Text(title).font(.caption).foregroundStyle(.secondary)
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
        Chart(samples) { s in
            AreaMark(x: .value("Time", s.at), y: .value("Value", value(s)))
                .foregroundStyle(color.opacity(0.15))
            LineMark(x: .value("Time", s.at), y: .value("Value", value(s)))
                .foregroundStyle(color)
                .lineStyle(StrokeStyle(lineWidth: 1.5))
        }
        .chartXAxis(.hidden)
        .chartYAxis(.hidden)
        .chartYScale(domain: .automatic(includesZero: true))
        .overlay {
            if samples.count < 2 {
                Text("collecting…").font(.caption2).foregroundStyle(.tertiary)
            }
        }
    }
}

private struct RuntimeTile: View {
    let metrics: SystemMetrics?

    var body: some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 10) {
                Text("orchd runtime").font(.headline)
                Gauge(title: "Goroutines", value: metrics.map { "\($0.goroutines)" } ?? "–") { EmptyView() }
                Gauge(title: "Go heap", value: metrics.map { formatBytes($0.heapBytes) } ?? "–") { EmptyView() }
            }
            .padding(4)
        }
        .frame(width: 160)
    }
}

private struct SubsystemsBox: View {
    let status: DaemonStatus?
    let restart: RestartModel

    var body: some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 8) {
                HStack {
                    Text("Subsystems").font(.headline)
                    Spacer()
                    if let status {
                        Text(verbatim: "orchd \(status.version) · up \(formatUptime(status.uptimeSeconds))")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                Divider()
                if let status {
                    // Ages tick every second between polls.
                    TimelineView(.periodic(from: .now, by: 1)) { context in
                        VStack(spacing: 6) {
                            ForEach(status.subsystems) { unit in
                                SubsystemLine(unit: unit, now: context.date, restart: restart)
                            }
                        }
                    }
                    if let message = restart.lastMessage {
                        Text(message)
                            .font(.caption)
                            .foregroundStyle(restart.lastFailed ? .red : .secondary)
                    }
                } else {
                    Text("Waiting for orchd…").foregroundStyle(.secondary)
                }
            }
            .padding(4)
        }
    }
}

private struct SubsystemLine: View {
    let unit: Subsystem
    let now: Date
    let restart: RestartModel

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Circle()
                .fill(unit.isFailed ? Color.red : unit.state == "running" ? .green : .secondary)
                .frame(width: 8, height: 8)
            Text(verbatim: unit.name).font(.body.monospaced()).frame(width: 170, alignment: .leading)
            Text(verbatim: unit.error.isEmpty ? unit.detail : unit.error)
                .font(.caption)
                .foregroundStyle(unit.error.isEmpty ? Color.secondary : Color.red)
                .lineLimit(1)
                .truncationMode(.tail)
                .help(unit.error.isEmpty ? unit.detail : unit.error)
            Spacer()
            Text(verbatim: beat)
                .font(.caption.monospacedDigit())
                .foregroundStyle(.secondary)
                .frame(width: 90, alignment: .trailing)
            Text(verbatim: unit.restarts > 0 ? "↻ \(unit.restarts)" : "")
                .font(.caption.monospacedDigit())
                .foregroundStyle(.secondary)
                .frame(width: 32, alignment: .trailing)
            if restart.restarting == unit.name {
                ProgressView().controlSize(.small).frame(width: 64)
            } else {
                Button("Restart") { Task { await restart.restart(unit.name) } }
                    .controlSize(.small)
                    .disabled(restart.restarting != nil)
                    .frame(width: 64)
            }
        }
    }

    private var beat: String {
        guard let last = unit.lastBeat else { return unit.isFailed ? "failed" : unit.state }
        return "beat \(formatUptime(Int64(now.timeIntervalSince(last)))) ago"
    }
}

private struct StorageBox: View {
    let metrics: SystemMetrics?

    var body: some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 6) {
                Text("Storage").font(.headline)
                Divider()
                ForEach(metrics?.data ?? []) { d in
                    HStack {
                        Text(verbatim: d.name).font(.body.monospaced())
                        Spacer()
                        Text(verbatim: formatBytes(d.bytes)).monospacedDigit()
                    }
                    .help(d.path)
                }
                if metrics == nil { Text("–").foregroundStyle(.secondary) }
            }
            .padding(4)
        }
        .frame(maxWidth: .infinity)
    }
}

private struct TasksBox: View {
    let metrics: SystemMetrics?

    var body: some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 6) {
                Text("Tasks").font(.headline)
                Divider()
                let counts = (metrics?.tasks ?? [:]).sorted { $0.key < $1.key }
                if counts.isEmpty {
                    Text(metrics == nil ? "–" : "none recorded").foregroundStyle(.secondary)
                }
                ForEach(counts, id: \.key) { status, n in
                    HStack {
                        Text(verbatim: status).font(.body.monospaced())
                        Spacer()
                        Text(verbatim: "\(n)").monospacedDigit()
                    }
                }
            }
            .padding(4)
        }
        .frame(maxWidth: .infinity)
    }
}

private struct ReportBox: View {
    let model: SystemModel

    var body: some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 10) {
                HStack {
                    Text("Status report").font(.headline)
                    if let report = model.report {
                        Text(report.problems == 0 ? "no problems" : "\(report.problems) problem\(report.problems == 1 ? "" : "s")")
                            .font(.caption)
                            .foregroundStyle(report.problems == 0 ? Color.secondary : Color.red)
                    }
                    Spacer()
                    if model.loadingReport { ProgressView().controlSize(.small) }
                    Text("orch status, every 15 s").font(.caption).foregroundStyle(.tertiary)
                }
                if let error = model.reportError {
                    Text(verbatim: error).font(.caption).foregroundStyle(.red).lineLimit(4)
                }
                // Titles and names can repeat (one line per harness), so rows
                // are identified by position.
                ForEach(Array((model.report?.sections ?? []).enumerated()), id: \.offset) { _, section in
                    VStack(alignment: .leading, spacing: 4) {
                        Text(verbatim: section.title.capitalized).font(.subheadline.weight(.semibold))
                        if let note = section.note {
                            Text(verbatim: note).font(.caption).foregroundStyle(.secondary)
                        }
                        ForEach(Array(section.lines.enumerated()), id: \.offset) { _, line in
                            ReportLine(line: line)
                        }
                    }
                    Divider()
                }
            }
            .padding(4)
        }
    }
}

private struct ReportLine: View {
    let line: StatusReport.Line

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: symbol).foregroundStyle(color).frame(width: 14)
            Text(verbatim: line.name).frame(width: 170, alignment: .leading)
            Text(verbatim: line.value).font(.body.monospaced())
            if let detail = line.detail {
                Text(verbatim: detail).font(.caption).foregroundStyle(.secondary).lineLimit(2)
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
