import OrchKit
import SwiftUI

/// "Relabel now" (SP-079, D-76): shown in the Spaces pane while stored
/// chunks are not labelled by the applied revision, during a relabel, and
/// with what the last run did. Absent with nothing stale. The pane hides it
/// in screen-share-safe mode, since the counts cover every space.
struct SpacesRelabelControl: View {
    let model: SpacesRelabelModel

    var body: some View {
        if model.isVisible {
            HStack(alignment: .top, spacing: 10) {
                icon.frame(width: 18)
                // The button sits under the text, so the text keeps its width
                // at the 320 pt companion width.
                VStack(alignment: .leading, spacing: 8) {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(verbatim: title).font(.callout.weight(.semibold))
                        Text(verbatim: detail).textStyle(.caption).fixedSize(horizontal: false, vertical: true)
                        if let failure = model.failure {
                            Label { Text(verbatim: failure) } icon: { Image(systemName: "exclamationmark.triangle.fill") }
                                .textStyle(.caption)
                                .foregroundStyle(Tone.bad.color)
                                .textSelection(.enabled)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        if let error = model.checkError, model.phase == .unknown {
                            Text(verbatim: error).textStyle(.caption).textSelection(.enabled)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                    }
                    .accessibilityElement(children: .combine)
                    action
                }
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .cardSurface()
        }
    }

    @ViewBuilder private var icon: some View {
        switch model.phase {
        case .running:
            ProgressView().controlSize(.small).accessibilityLabel("Relabelling")
        case .upToDate:
            Image(systemName: "checkmark.circle.fill").foregroundStyle(Tone.good.color).accessibilityHidden(true)
        case .stale:
            Image(systemName: "arrow.triangle.2.circlepath").foregroundStyle(Tone.accent.color).accessibilityHidden(true)
        case .unknown:
            Image(systemName: "exclamationmark.triangle").foregroundStyle(Tone.warning.color).accessibilityHidden(true)
        }
    }

    @ViewBuilder private var action: some View {
        switch model.phase {
        case .stale:
            // An unstructured task, so leaving the pane does not cancel the run.
            HStack(spacing: 8) {
                Button("Relabel Now") { Task { await model.start() } }
                    .disabled(!model.canStart)
                    .help("Place stored content again under the applied policy, so sessions can read it. Nothing is re-embedded.")
                // Disabled while orchd counts again, so a press never starts
                // a run on a count that is out of date.
                if model.isChecking {
                    ProgressView().controlSize(.small).accessibilityLabel("Counting stale content")
                }
            }
        case .unknown:
            Button("Check Again") { Task { await model.check() } }
                .disabled(model.isChecking)
        case .running, .upToDate:
            EmptyView()
        }
    }

    private var title: String {
        switch model.phase {
        case .stale(let n):
            n == 1 ? "1 stored chunk is labelled for an older policy"
                : "\(n.formatted()) stored chunks are labelled for an older policy"
        case .running(let n):
            n == 1 ? "Relabelling 1 chunk…" : "Relabelling \(n.formatted()) chunks…"
        case .upToDate:
            "All stored content is labelled for rev \(model.revision)"
        case .unknown:
            "Stored content could not be checked"
        }
    }

    private var detail: String {
        switch model.phase {
        case .stale:
            "Sessions cannot read it until it is relabelled for rev \(model.revision). orchd does not relabel on its own after an apply."
        case .running:
            "orch places each chunk under the applied policy and moves it to its space. This can take a few minutes; leaving the pane does not stop it."
        case .upToDate:
            lastRunSummary ?? "Nothing needs relabelling."
        case .unknown:
            "orch could not count the content stored under an older policy."
        }
    }

    private var lastRunSummary: String? {
        guard let run = model.lastRun else { return nil }
        let changed = run.changed == 1 ? "1 chunk" : "\(run.changed.formatted()) chunks"
        guard run.moved > 0 else { return "The last relabel updated \(changed)." }
        return "The last relabel updated \(changed); \(run.moved.formatted()) moved to their space's collection."
    }
}
