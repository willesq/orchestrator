import Foundation

/// How a settings item reads in a list: a short name a person recognizes,
/// the exact text it stands for, and an optional matcher. A hook's command
/// can be a long shell script, so the name is derived from what it runs.
public struct SettingDisplay: Sendable, Equatable {
    /// A readable name, such as "Orca" or "orch-hook session-start".
    public let title: String
    /// The full text the item stands for (a command, a rule), shown
    /// truncated under the title and in full in the detail pane.
    public let detail: String
    /// A hook's tool matcher, such as "Bash"; empty when it applies to all.
    public let matcher: String
    /// The title is itself a literal (a permission rule such as
    /// `Bash(git *)`), so it is shown in monospace.
    public let literalTitle: Bool
    /// The detail is a sentence (an instruction module's description), not
    /// a command or rule, so it is shown as prose rather than code.
    public let proseDetail: Bool

    public init(title: String, detail: String = "", matcher: String = "", literalTitle: Bool = false,
                proseDetail: Bool = false) {
        self.title = title
        self.detail = detail
        self.matcher = matcher
        self.literalTitle = literalTitle
        self.proseDetail = proseDetail
    }

    /// Whether the item matches a filter typed by the user. An empty filter
    /// matches everything.
    public func matches(_ query: String) -> Bool {
        let q = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !q.isEmpty else { return true }
        return [title, detail, matcher].contains { $0.localizedCaseInsensitiveContains(q) }
    }
}

extension SettingItem {
    /// The item as a list shows it. Every string is made safe to show.
    public var display: SettingDisplay {
        let fallback = displaySafe(summary.isEmpty ? ref.identity : summary)
        switch ref.kind {
        case "hook": break
        case "permission": return SettingDisplay(title: fallback, literalTitle: true)
        case "rule": return RulePayload(json: payloadJSON)?.display ?? SettingDisplay(title: fallback)
        case "module": return ModulePayload(json: payloadJSON)?.display ?? SettingDisplay(title: fallback)
        default: return SettingDisplay(title: fallback)
        }
        guard let hook = HookPayload(json: payloadJSON) else { return SettingDisplay(title: fallback, detail: fallback) }
        let condition = hook.condition.isEmpty ? "" : " (if \(hook.condition))"
        return SettingDisplay(
            title: displaySafe(hook.name),
            detail: displaySafe(hook.what + condition),
            matcher: displaySafe(hook.matcher == "*" ? "" : hook.matcher)
        )
    }
}

extension SettingItem {
    /// The instruction module this item is, or nil for any other kind.
    public var module: ModulePayload? { ref.kind == "module" ? ModulePayload(json: payloadJSON) : nil }
}

/// An instruction module's payload: what it is, where it installs, and the
/// full markdown agents read, so the detail pane can show it.
public struct ModulePayload: Sendable, Equatable {
    public let title: String
    public let description: String
    public let body: String
    public let path: String
    /// Part of the agents' security policy; the app confirms turning it off.
    public let sensitive: Bool

    public init?(json: String) {
        guard let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let title = object["title"] as? String, !title.isEmpty else { return nil }
        self.title = title
        description = object["description"] as? String ?? ""
        body = object["body"] as? String ?? ""
        path = object["path"] as? String ?? ""
        sensitive = object["sensitive"] as? Bool ?? false
    }

    var display: SettingDisplay {
        SettingDisplay(title: displaySafe(title), detail: displaySafe(description), proseDetail: true)
    }
}

/// The parts of a cc-safety-net rule's payload a list needs.
struct RulePayload {
    let rule: String
    let command: String
    let blocks: [String]

    init?(json: String) {
        guard let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let rule = object["rule"] as? String, !rule.isEmpty else { return nil }
        self.rule = rule
        command = object["command"] as? String ?? ""
        blocks = object["block_args"] as? [String] ?? []
    }

    var display: SettingDisplay {
        let detail = command.isEmpty ? "" : "blocks " + SafetyNetRule(name: rule, command: command, blockArgs: blocks).blockedPhrase
        return SettingDisplay(title: displaySafe(rule), detail: displaySafe(detail))
    }
}

/// The parts of a hook's payload a list needs.
struct HookPayload {
    let matcher: String
    let condition: String
    /// What the handler runs: a command, a prompt, a URL or an MCP tool.
    let what: String
    let type: String

    init?(json: String) {
        guard let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let handler = object["handler"] as? [String: Any] else { return nil }
        matcher = object["matcher"] as? String ?? ""
        condition = handler["if"] as? String ?? ""
        let string = { (key: String) in (handler[key] as? String) ?? "" }
        if !string("command").isEmpty {
            what = string("command")
            type = "command"
        } else if !string("prompt").isEmpty {
            what = string("prompt")
            type = "prompt"
        } else if !string("url").isEmpty {
            what = string("url")
            type = "url"
        } else {
            what = "\(string("server")) \(string("tool"))".trimmingCharacters(in: .whitespaces)
            type = "mcp"
        }
    }

    /// The readable name for what the hook runs.
    var name: String {
        switch type {
        case "command": HookName.of(command: what)
        case "prompt": "Prompt"
        case "url": URL(string: what)?.host.map { "Webhook \($0)" } ?? "Webhook"
        default: what.isEmpty ? "Hook" : what
        }
    }
}

/// Names a hook command after the tool it runs.
public enum HookName {
    /// Tools recognized anywhere in a command, in the order they are checked,
    /// with the name shown for each. A wrapper script that mentions one of
    /// these (Orca's installs a long shell script) is named after it.
    static let known: [(needle: String, name: String)] = [
        (".orca/agent-hooks", "Orca"),
        ("cc-safety-net", "cc-safety-net"),
        ("cc-status", "cc-status"),
    ]

    /// Tools named with their first argument, because one binary serves
    /// several hooks ("orch-hook session-start", "serena-hooks remind").
    static let withSubcommand: Set<String> = ["orch-hook", "serena-hooks"]

    /// Shell keywords that start a compound command, such as the wrapper
    /// script Orca installs; the tool it runs is somewhere inside.
    static let compoundStarts: Set<String> = ["if", "case", "for", "while", "until", "{", "(", "[", "[["]

    /// A readable name for a shell command: a known tool, else the base name
    /// of the program it runs. Known tools are matched on the program itself,
    /// or anywhere in a compound command, never in a trailing comment, so a
    /// command cannot borrow a trusted name by mentioning it.
    public static func of(command: String) -> String {
        // Words from a "#" on are a comment, not part of what runs.
        let words = Array(shellWords(command).prefix { !$0.hasPrefix("#") })
        // Leading VAR=value assignments set the environment; the program follows.
        guard let index = words.firstIndex(where: { !isAssignment($0) }) else {
            return words.first ?? command
        }
        let programWord = words[index]
        let haystack = compoundStarts.contains(programWord) ? words.joined(separator: " ") : programWord
        for (needle, name) in known where haystack.contains(needle) { return name }
        let program = (programWord as NSString).lastPathComponent
        if program == "rtk" { return "rtk" }
        if withSubcommand.contains(program), index + 1 < words.count, !words[index + 1].hasPrefix("-") {
            return "\(program) \(words[index + 1])"
        }
        return program.isEmpty ? command : program
    }

    private static func isAssignment(_ word: String) -> Bool {
        guard let eq = word.firstIndex(of: "="), eq != word.startIndex else { return false }
        return word[..<eq].allSatisfy { $0 == "_" || $0.isLetter || $0.isNumber }
    }

    /// Splits on whitespace outside quotes and drops the quotes: enough to
    /// find a program name, not a full shell parser.
    static func shellWords(_ s: String) -> [String] {
        var words: [String] = []
        var current = ""
        var quote: Character?
        for ch in s {
            if let q = quote {
                if ch == q { quote = nil } else { current.append(ch) }
            } else if ch == "\"" || ch == "'" {
                quote = ch
            } else if ch.isWhitespace {
                if !current.isEmpty { words.append(current); current = "" }
            } else {
                current.append(ch)
            }
        }
        if !current.isEmpty { words.append(current) }
        return words
    }
}
