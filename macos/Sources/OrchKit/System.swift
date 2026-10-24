import Foundation

/// The orchd calls the System tab uses.
public protocol SystemAPI: Sendable {
    func status() async throws -> DaemonStatus
    /// Log lines after `after`, from every source, oldest first.
    func tailLogs(after: UInt64, limit: Int) async throws -> LogPage
    func systemMetrics() async throws -> SystemMetrics
}

/// One line orchd logged. The sequence number is unique for orchd's lifetime.
public struct LogLine: Sendable, Equatable, Identifiable {
    public var id: UInt64 { seq }
    public let seq: UInt64
    public let at: Date
    public let source: String
    public let message: String

    public init(seq: UInt64, at: Date, source: String, message: String) {
        self.seq = seq
        self.at = at
        self.source = source
        self.message = message
    }
}

public struct LogPage: Sendable, Equatable {
    public let lines: [LogLine]
    /// The newest sequence orchd has logged; the next poll asks for lines after it.
    public let lastSeq: UInt64
    /// The oldest sequence orchd still keeps.
    public let oldestSeq: UInt64
    public let sources: [String]

    public init(lines: [LogLine], lastSeq: UInt64, oldestSeq: UInt64, sources: [String]) {
        self.lines = lines
        self.lastSeq = lastSeq
        self.oldestSeq = oldestSeq
        self.sources = sources
    }
}

/// Resource use of one OS process orchd runs or depends on.
public struct ProcessStats: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    public let pid: Int
    public let rssBytes: UInt64
    /// Of one core, so a busy multithreaded process can pass 100.
    public let cpuPercent: Double
    public let uptimeSeconds: Int64
    public let running: Bool

    public init(name: String, pid: Int = 0, rssBytes: UInt64 = 0, cpuPercent: Double = 0,
                uptimeSeconds: Int64 = 0, running: Bool = false) {
        self.name = name
        self.pid = pid
        self.rssBytes = rssBytes
        self.cpuPercent = cpuPercent
        self.uptimeSeconds = uptimeSeconds
        self.running = running
    }
}

/// The on-disk size of one of orchd's stores.
public struct DataUsage: Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    public let path: String
    public let bytes: UInt64

    public init(name: String, path: String, bytes: UInt64) {
        self.name = name
        self.path = path
        self.bytes = bytes
    }
}

public struct SystemMetrics: Sendable, Equatable {
    public let at: Date
    public let processes: [ProcessStats]
    public let goroutines: Int
    public let heapBytes: UInt64
    public let data: [DataUsage]
    /// Task counts by status; empty when orchd could not count them.
    public let tasks: [String: Int]

    public init(at: Date, processes: [ProcessStats], goroutines: Int = 0, heapBytes: UInt64 = 0,
                data: [DataUsage] = [], tasks: [String: Int] = [:]) {
        self.at = at
        self.processes = processes
        self.goroutines = goroutines
        self.heapBytes = heapBytes
        self.data = data
        self.tasks = tasks
    }

    public func process(_ name: String) -> ProcessStats? { processes.first { $0.name == name } }
}

/// `orch status --json`: every component's state, including the checks that
/// work with orchd down.
public struct StatusReport: Sendable, Equatable, Decodable {
    public struct Line: Sendable, Equatable, Decodable, Identifiable {
        public var id: String { name }
        public let name: String
        public let value: String
        public let detail: String?
        /// `ok`, `warn` or `bad`; absent for plain information.
        public let level: String?

        public init(name: String, value: String, detail: String? = nil, level: String? = nil) {
            self.name = name
            self.value = value
            self.detail = detail
            self.level = level
        }
    }

    public struct Section: Sendable, Equatable, Decodable, Identifiable {
        public var id: String { title }
        public let title: String
        public let note: String?
        public let lines: [Line]

        public init(title: String, note: String? = nil, lines: [Line] = []) {
            self.title = title
            self.note = note
            self.lines = lines
        }

        enum CodingKeys: String, CodingKey { case title, note, lines }

        public init(from decoder: any Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            title = try c.decode(String.self, forKey: .title)
            note = try c.decodeIfPresent(String.self, forKey: .note)
            lines = try c.decodeIfPresent([Line].self, forKey: .lines) ?? []
        }
    }

    public let version: String
    public let problems: Int
    public let sections: [Section]

    public init(version: String, problems: Int, sections: [Section]) {
        self.version = version
        self.problems = problems
        self.sections = sections
    }

    enum CodingKeys: String, CodingKey { case version, problems, sections }

    /// Lenient, so a field an older or newer orch leaves out costs that field,
    /// not the whole report.
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        version = try c.decodeIfPresent(String.self, forKey: .version) ?? ""
        problems = try c.decodeIfPresent(Int.self, forKey: .problems) ?? 0
        sections = try c.decodeIfPresent([Section].self, forKey: .sections) ?? []
    }
}

/// Formats a byte count the way Finder does: `1.2 GB`, `340 MB`.
public func formatBytes(_ bytes: UInt64) -> String {
    let f = ByteCountFormatter()
    f.countStyle = .file
    // "0 bytes", not "Zero KB", so a column of sizes stays numeric.
    f.allowsNonnumericFormatting = false
    return f.string(fromByteCount: Int64(clamping: bytes))
}
