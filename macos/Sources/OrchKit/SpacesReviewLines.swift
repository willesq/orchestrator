import Foundation

/// The review lines of a draft against the applied revision, and the
/// effect counts read from them.
extension PolicyDoc {
    func lines(problems: [PolicyTarget: [SpacesProblemInfo]]) -> [ReviewLine] {
        var out: [ReviewLine] = []
        func line(_ id: String, _ change: ReviewLine.Change, space: String, field: String, text: String, nested: Bool,
                  target: PolicyTarget?) {
            out.append(ReviewLine(id: id, change: change, space: displaySafe(space), field: field, text: displaySafe(text),
                                  nested: nested, problems: target.flatMap { problems[$0] } ?? [],
                                  excludable: target.map { droppable($0, problems[$0] ?? []) } ?? false))
        }
        for i in draftSpaces.indices {
            let s = draftSpaces[i]
            let name = Self.name(s)
            let new = isNew(i)
            line("s\(i)", new ? .added : .same, space: name, field: "", text: name.isEmpty ? s.text : name, nested: false,
                 target: .space(i))
            let old = appliedSpace(i)
            for f in Self.listFields {
                let values = s[f]?.arrayValue ?? []
                let flags = added(i, f)
                for (j, v) in values.enumerated() {
                    line("s\(i)/\(f)/\(j)", flags[j] ? .added : .same, space: name, field: f, text: "\(f): \(v.display)",
                         nested: true, target: .value(i, f, j))
                }
                for (k, v) in removed(i, f).enumerated() {
                    line("s\(i)/\(f)/-\(k)", .removed, space: name, field: f, text: "\(f): \(v.display)", nested: true,
                         target: nil)
                }
            }
            for k in settingKeys(s, old) {
                let now = s[k], was = old?[k]
                if let was, was != now {
                    line("s\(i)/\(k)/-", .removed, space: name, field: k, text: "\(k): \(was.display)", nested: true,
                         target: nil)
                }
                if let now {
                    line("s\(i)/\(k)", now == was ? .same : .added, space: name, field: k, text: "\(k): \(now.display)",
                         nested: true, target: .field(i, k))
                }
            }
        }
        let kept = Set(draftSpaces.map(Self.name))
        for (r, s) in appliedSpaces.enumerated() where !kept.contains(Self.name(s)) {
            let name = Self.name(s)
            line("r\(r)", .removed, space: name, field: "", text: name, nested: false, target: nil)
            for f in Self.listFields {
                for (j, v) in (s[f]?.arrayValue ?? []).enumerated() {
                    line("r\(r)/\(f)/\(j)", .removed, space: name, field: f, text: "\(f): \(v.display)", nested: true,
                         target: nil)
                }
            }
            for k in settingKeys(s, nil) {
                if let v = s[k] {
                    line("r\(r)/\(k)", .removed, space: name, field: k, text: "\(k): \(v.display)", nested: true, target: nil)
                }
            }
        }
        for k in policyKeys() {
            let now = draft[k], was = applied[k]
            if let was, was != now {
                line("p/\(k)/-", .removed, space: "", field: k, text: "\(k): \(was.display)", nested: false, target: nil)
            }
            if let now {
                line("p/\(k)", now == was ? .same : .added, space: "", field: k, text: "\(k): \(now.display)",
                     nested: false, target: .policyField(k))
            }
        }
        return out
    }

    /// A space's settings, known ones first, then any other field.
    private func settingKeys(_ s: JSONValue, _ old: JSONValue?) -> [String] {
        var keys = Set<String>()
        for v in [s, old ?? .null] {
            if case .object(let o) = v { keys.formUnion(o.keys) }
        }
        keys.subtract(Self.listFields + ["name"])
        return Self.fixedFields.filter(keys.contains) + keys.subtracting(Self.fixedFields).sorted()
    }

    /// The policy's own fields besides its spaces.
    private func policyKeys() -> [String] {
        var keys = Set<String>()
        for v in [draft, applied] {
            if case .object(let o) = v { keys.formUnion(o.keys) }
        }
        keys.remove("spaces")
        return keys.sorted()
    }

    /// Spaces added, removed and changed, lines added and removed, and the
    /// lines that would be dropped.
    static func effects(_ lines: [ReviewLine]) -> [ReviewEffect] {
        let heads = lines.filter { $0.field.isEmpty && !$0.nested && $0.id.first != "p" }
        let bodies = lines.filter { !($0.field.isEmpty && !$0.nested) }
        var changed: Set<String> = []
        for l in bodies where l.change != .same && !l.space.isEmpty { changed.insert(l.space) }
        let sameHeads = heads.filter { $0.change == .same }.map(\.space)
        return [
            ReviewEffect(label: "Spaces added", count: heads.filter { $0.change == .added }.count),
            ReviewEffect(label: "Spaces removed", count: heads.filter { $0.change == .removed }.count),
            ReviewEffect(label: "Spaces changed", count: sameHeads.filter(changed.contains).count),
            ReviewEffect(label: "Bindings and settings added", count: bodies.filter { $0.change == .added }.count),
            ReviewEffect(label: "Bindings and settings removed", count: bodies.filter { $0.change == .removed }.count),
            ReviewEffect(label: "Invalid lines", count: lines.filter(\.invalid).count),
        ]
    }
}
