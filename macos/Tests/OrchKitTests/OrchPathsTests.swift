import Testing
@testable import OrchKit

@Suite struct OrchPathsTests {
    @Test func defaultsUnderLocalShare() {
        #expect(OrchPaths.socketPath(environment: [:], home: "/Users/u") == "/Users/u/.local/share/orch/orchd.sock")
    }

    @Test func socketOverrideWins() {
        let env = ["ORCH_SOCKET": "/tmp/s.sock", "ORCH_DATA_DIR": "/data"]
        #expect(OrchPaths.socketPath(environment: env, home: "/Users/u") == "/tmp/s.sock")
    }

    @Test func dataDirOverride() {
        #expect(OrchPaths.socketPath(environment: ["ORCH_DATA_DIR": "/data"], home: "/Users/u") == "/data/orchd.sock")
    }

    @Test func xdgDataHome() {
        #expect(OrchPaths.dataDir(environment: ["XDG_DATA_HOME": "/xdg"], home: "/Users/u") == "/xdg/orch")
    }

    @Test func emptyOverridesAreIgnored() {
        let env = ["ORCH_SOCKET": "", "ORCH_DATA_DIR": "", "XDG_DATA_HOME": ""]
        #expect(OrchPaths.socketPath(environment: env, home: "/Users/u") == "/Users/u/.local/share/orch/orchd.sock")
    }
}
