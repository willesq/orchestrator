import Foundation

/// Locations shared with the Go binaries. These mirror `internal/paths` so the
/// app and `orch` always agree on which daemon they talk to.
public enum OrchPaths {
    /// The orchd socket: `ORCH_SOCKET`, else `<data dir>/orchd.sock`.
    public static func socketPath(
        environment: [String: String] = ProcessInfo.processInfo.environment,
        home: String = NSHomeDirectory()
    ) -> String {
        if let socket = environment["ORCH_SOCKET"], !socket.isEmpty {
            return socket
        }
        return (dataDir(environment: environment, home: home) as NSString)
            .appendingPathComponent("orchd.sock")
    }

    /// The data dir: `ORCH_DATA_DIR`, else `$XDG_DATA_HOME/orch`, else
    /// `~/.local/share/orch`.
    public static func dataDir(
        environment: [String: String] = ProcessInfo.processInfo.environment,
        home: String = NSHomeDirectory()
    ) -> String {
        if let dir = environment["ORCH_DATA_DIR"], !dir.isEmpty {
            return dir
        }
        var base = environment["XDG_DATA_HOME"] ?? ""
        if base.isEmpty {
            base = (home as NSString).appendingPathComponent(".local/share")
        }
        return (base as NSString).appendingPathComponent("orch")
    }
}
