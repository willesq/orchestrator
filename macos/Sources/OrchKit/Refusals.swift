import Foundation
import Observation

/// One tool call the spaces guard or the outbound terms check refused, as
/// orch-hook reports it on the operator's spaces stream (spaces plan
/// SP-048). It names the call, never what it held: no command, path or term.
public struct Refusal: Equatable, Sendable {
    public var session: String
    public var space: String
    public var tool: String
    public var rule: String
    /// The term an outbound_term refusal found, by its space, line and hash.
    public var term: TermRef?
    public var at: Date?

    public struct TermRef: Equatable, Sendable {
        public var space: String
        public var line: Int
        public var hash: String

        public init(space: String, line: Int, hash: String) {
            self.space = space
            self.line = line
            self.hash = hash
        }
    }

    public init(session: String, space: String, tool: String, rule: String, term: TermRef? = nil, at: Date? = nil) {
        self.session = session
        self.space = space
        self.tool = tool
        self.rule = rule
        self.term = term
        self.at = at
    }

    /// Where the refusal happened, for a notification's subtitle.
    public var place: String {
        "\(space.isEmpty ? "no space" : "space \(space)") · \(tool)"
    }

    /// The rule, session and term, for a notification's body.
    public var detail: String {
        var s = "Rule \(rule)"
        if !session.isEmpty { s += " in session \(session)" }
        if let term { s += "; a term of space \(term.space), terms file line \(term.line) (hash \(term.hash))" }
        return s + "."
    }
}

/// Reads refusals appended to the spaces stream (`<data dir>/events/spaces.jsonl`)
/// since a byte offset. Any process running as the user can append to the
/// stream, so each field must have the shape orch writes or the line is
/// skipped, and only whole lines are taken.
public struct RefusalStream: Sendable {
    public let path: String
    /// The most read in one call; the rest waits for the next.
    public var maxRead = 1 << 20

    public init(path: String) { self.path = path }

    /// The stream at its default place.
    public static func spaces(environment: [String: String] = ProcessInfo.processInfo.environment,
                              home: String = NSHomeDirectory()) -> RefusalStream {
        let dir = (OrchPaths.dataDir(environment: environment, home: home) as NSString).appendingPathComponent("events")
        return RefusalStream(path: (dir as NSString).appendingPathComponent("spaces.jsonl"))
    }

    /// The stream's size now: where reading starts, so refusals from before
    /// the app started are not announced.
    public func end() -> UInt64 {
        ((try? FileManager.default.attributesOfItem(atPath: path))?[.size] as? NSNumber)?.uint64Value ?? 0
    }

    /// Refusals in whole lines after offset, and the offset to read from
    /// next. A stream shorter than offset was replaced and is read from its
    /// start.
    public func read(from offset: UInt64) -> (refusals: [Refusal], next: UInt64) {
        guard let handle = FileHandle(forReadingAtPath: path) else { return ([], 0) }
        defer { try? handle.close() }
        let size = (try? handle.seekToEnd()) ?? 0
        var start = offset
        if size < start { start = 0 }
        guard size > start, (try? handle.seek(toOffset: start)) != nil,
              let data = try? handle.read(upToCount: maxRead), !data.isEmpty
        else { return ([], start) }
        guard let lastNewline = data.lastIndex(of: UInt8(ascii: "\n")) else {
            // A line longer than maxRead is no line orch writes: skip it.
            return ([], data.count >= maxRead ? start + UInt64(data.count) : start)
        }
        let whole = data[data.startIndex...lastNewline]
        let refusals = whole.split(separator: UInt8(ascii: "\n")).compactMap { Self.parse(Data($0)) }
        return (refusals, start + UInt64(whole.count))
    }

    private static let session = try! NSRegularExpression(pattern: "^[A-Za-z0-9_-]{1,128}$")
    private static let space = try! NSRegularExpression(pattern: "^[a-z][a-z0-9_-]{0,31}$")
    private static let tool = try! NSRegularExpression(pattern: "^[A-Za-z][A-Za-z0-9_.:-]{0,127}$")
    private static let rule = try! NSRegularExpression(pattern: "^[a-z][a-z_]{0,39}$")
    private static let hash = try! NSRegularExpression(pattern: "^[0-9a-f]{1,64}$")

    private static func matches(_ re: NSRegularExpression, _ s: String) -> Bool {
        re.firstMatch(in: s, range: NSRange(s.startIndex..., in: s)) != nil
    }

    private struct Line: Decodable {
        var kind: String
        var timestamp: String?
        var refusal: Body?

        struct Body: Decodable {
            var session: String?
            var space: String?
            var tool: String
            var rule: String
            var term: Term?
        }

        struct Term: Decodable {
            var space: String
            var line: Int
            var hash: String
        }
    }

    /// One stream line as a refusal; nil for another kind of event or a
    /// field orch would not write.
    static func parse(_ line: Data) -> Refusal? {
        guard let l = try? JSONDecoder().decode(Line.self, from: line), l.kind == "refusal", let b = l.refusal else { return nil }
        let session = b.session ?? "", space = b.space ?? ""
        guard session.isEmpty || matches(Self.session, session),
              space.isEmpty || matches(Self.space, space),
              matches(Self.tool, b.tool), matches(Self.rule, b.rule)
        else { return nil }
        var term: Refusal.TermRef?
        if let t = b.term {
            guard matches(Self.space, t.space), t.line > 0, matches(Self.hash, t.hash) else { return nil }
            term = Refusal.TermRef(space: t.space, line: t.line, hash: t.hash)
        }
        let at = l.timestamp.flatMap { try? Date($0, strategy: .iso8601) }
        return Refusal(session: session, space: space, tool: b.tool, rule: b.rule, term: term, at: at)
    }
}

/// Follows the spaces stream for refusals made while the app runs.
@MainActor
@Observable
public final class RefusalMonitor {
    /// The latest refusals, newest last, at most `kept`.
    public private(set) var recent: [Refusal] = []
    /// Called with the refusals each poll found.
    public var onNewRefusals: (@MainActor ([Refusal]) -> Void)?

    public let kept = 20
    private let stream: RefusalStream
    private let interval: Duration
    private var offset: UInt64?
    private var pollTask: Task<Void, Never>?

    public init(stream: RefusalStream = .spaces(), interval: Duration = .seconds(5)) {
        self.stream = stream
        self.interval = interval
    }

    /// Starts polling; calling it again while polling is a no-op.
    public func start() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                self?.poll()
                guard let interval = self?.interval else { return }
                try? await Task.sleep(for: interval)
            }
        }
    }

    public func stop() {
        pollTask?.cancel()
        pollTask = nil
    }

    /// Reads what was appended since the last poll. The first poll only
    /// notes where the stream ends.
    public func poll() {
        guard let from = offset else {
            offset = stream.end()
            return
        }
        let (found, next) = stream.read(from: from)
        offset = next
        guard !found.isEmpty else { return }
        recent = Array((recent + found).suffix(kept))
        onNewRefusals?(found)
    }
}

/// How to announce refusals: one notification each, or a single summary
/// once there are more than `limit`, so a burst never stacks banners.
public enum RefusalNotificationPlan: Equatable, Sendable {
    case each([Refusal])
    case summary(count: Int, spaces: [String])

    public static let defaultLimit = 3

    public init(_ refusals: [Refusal], limit: Int = defaultLimit) {
        if refusals.count > limit {
            var seen = Set<String>()
            let spaces = refusals.map { $0.space.isEmpty ? "no space" : $0.space }.filter { seen.insert($0).inserted }
            self = .summary(count: refusals.count, spaces: spaces)
        } else {
            self = .each(refusals)
        }
    }
}
