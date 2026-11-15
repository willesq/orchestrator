import Foundation

/// What spaces enforce for one coding-agent harness (the Coverage editor,
/// SP-053): what its hooks can do, as recorded in docs/internals/hooks.md,
/// and what orchd saw of it (ListSessions).
public struct HarnessCoverage: Sendable, Equatable, Identifiable {
    public var harness: String
    public var title: String
    /// SessionStart pins sessions to a space.
    public var pins: Bool
    /// PreToolUse runs the tool guard and the outbound terms check.
    public var toolGuard: Bool
    /// CwdChanged reports directory moves.
    public var directoryMoves: Bool
    public var note: String
    /// Live sessions holding a pin.
    public var pinnedSessions: Int
    /// Running processes holding no pin: their tool calls go unchecked.
    public var unpinnedProcesses: Int
    public var toolEvents: Int64
    public var refusals: Int64

    public var id: String { harness }

    /// Whether every running process of the harness is covered.
    public var gap: Bool { unpinnedProcesses > 0 }

    /// The three harnesses orch installs into, with what each one's hooks
    /// give spaces, and the counts `sessions` shows for each.
    public static func table(sessions: [PinnedSession]) -> [HarnessCoverage] {
        let known: [HarnessCoverage] = [
            HarnessCoverage(harness: "claude", title: "Claude Code", pins: true, toolGuard: true, directoryMoves: true,
                            note: "Every tool call is checked before it runs, and each directory change is logged."),
            HarnessCoverage(harness: "codex", title: "Codex", pins: true, toolGuard: true, directoryMoves: false,
                            note: "Every tool call is checked before it runs; Codex reports no directory changes, so each call's own folder is checked."),
            HarnessCoverage(harness: "gemini", title: "Gemini CLI", pins: false, toolGuard: false, directoryMoves: false,
                            note: "Not covered (D-20): its sessions get no space and its tool calls are not checked, so a running one always shows as unpinned."),
        ]
        var out = known.map { row in
            var row = row
            row.count(sessions.filter { $0.harness == row.harness })
            return row
        }
        let others = Set(sessions.map(\.harness)).subtracting(known.map(\.harness)).sorted()
        for h in others {
            var row = HarnessCoverage(harness: h, title: displaySafe(h), pins: false, toolGuard: false, directoryMoves: false,
                                      note: "orch does not install hooks for this harness.")
            row.count(sessions.filter { $0.harness == h })
            out.append(row)
        }
        return out
    }

    init(harness: String, title: String, pins: Bool, toolGuard: Bool, directoryMoves: Bool, note: String) {
        self.harness = harness
        self.title = title
        self.pins = pins
        self.toolGuard = toolGuard
        self.directoryMoves = directoryMoves
        self.note = note
        pinnedSessions = 0
        unpinnedProcesses = 0
        toolEvents = 0
        refusals = 0
    }

    private mutating func count(_ sessions: [PinnedSession]) {
        let live = sessions.filter { $0.state != "ended" }
        pinnedSessions = live.filter { $0.state != "unpinned" }.count
        unpinnedProcesses = live.filter { $0.state == "unpinned" }.count
        toolEvents = sessions.reduce(0) { $0 + $1.toolEvents }
        refusals = sessions.reduce(0) { $0 + $1.refusals }
    }
}
