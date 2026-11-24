import OrchKit
import SwiftUI

/// How a PR's reviews are set up: where the reviewing agent's project
/// instructions come from, what it compares the PR against, and whether the
/// repo's Claude Code settings, hooks and MCP servers apply. The card decides
/// what a change does; this view only reports it.
struct ReviewSetupSection: View {
    let setup: PRReviewSetup
    let change: (PRReviewSetup) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Review setup").textStyle(.caption)
            Grid(alignment: .leading, horizontalSpacing: 8, verticalSpacing: 4) {
                GridRow {
                    Text("Project instructions from")
                    Picker("Project instructions from", selection: binding(\.instructionsFrom)) {
                        ForEach(PRReviewInstructionsSource.allCases, id: \.self) { Text($0.label).tag($0) }
                    }
                    .labelsHidden()
                    .fixedSize()
                    .help(setup.instructionsFrom.explanation)
                }
                GridRow {
                    Text("Compare against")
                    Picker("Compare against", selection: binding(\.compareAgainst)) {
                        ForEach(PRReviewComparison.allCases, id: \.self) { Text($0.label).tag($0) }
                    }
                    .labelsHidden()
                    .fixedSize()
                    .help("PR base reviews just this PR. Default branch reviews everything it would merge, including a stacked parent.")
                }
                GridRow {
                    Text("Repo settings, hooks and MCP")
                    Picker("Repo settings, hooks and MCP", selection: binding(\.projectHooks)) {
                        ForEach(PRReviewProjectHooks.allCases, id: \.self) { Text($0.label).tag($0) }
                    }
                    .labelsHidden()
                    .fixedSize()
                    .help("Automatic applies them to your own repos and repos bound to one of your spaces. Otherwise orch removes them from its checkout; instructions, skills and agents still load")
                }
            }
            Text(verbatim: setup.instructionsFrom.explanation).textStyle(.caption)
        }
    }

    private func binding<Value>(_ key: WritableKeyPath<PRReviewSetup, Value>) -> Binding<Value> {
        Binding(get: { setup[keyPath: key] }, set: { value in
            var next = setup
            next[keyPath: key] = value
            if next != setup { change(next) }
        })
    }
}
