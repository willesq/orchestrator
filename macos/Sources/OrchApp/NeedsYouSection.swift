import OrchKit
import SwiftUI

/// The tray's list of PRs waiting on the user, most urgent first.
struct NeedsYouSection: View {
    let monitor: PRMonitor

    private let shown = 8

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("Needs you").font(.subheadline.weight(.semibold))
                Spacer()
                if let updated = monitor.list?.updatedAt {
                    Text("updated \(updated, format: .relative(presentation: .named))")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
            let prs = monitor.needsYou
            if monitor.list == nil {
                Text(monitor.lastError.map { "Could not load PRs: \($0)" } ?? "Loading PRs…")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else if prs.isEmpty {
                Label("Nothing needs you", systemImage: "checkmark.circle")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            } else {
                ForEach(prs.prefix(shown)) { pr in
                    PRRow(pr: pr)
                }
                if prs.count > shown {
                    Text("and \(prs.count - shown) more (orch prs --needs-me)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }
}

private struct PRRow: View {
    let pr: PullRequest

    var body: some View {
        Button {
            if let url = pr.webURL { NSWorkspace.shared.open(url) }
        } label: {
            HStack(alignment: .top, spacing: 8) {
                Image(systemName: pr.attention?.symbolName ?? "circle")
                    .foregroundStyle(tint)
                    .frame(width: 16)
                VStack(alignment: .leading, spacing: 1) {
                    Text(pr.title).font(.callout).lineLimit(1)
                    Text("\(pr.repo) · \(pr.attention?.label ?? "")")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
                Spacer(minLength: 0)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(pr.webURL == nil)
        .help(pr.url)
    }

    private var tint: Color {
        switch pr.attention {
        case .ciFailed, .changesRequested, .conflicts: .red
        case .reviewRequested: .blue
        case .readyToMerge: .green
        case nil: .secondary
        }
    }
}
