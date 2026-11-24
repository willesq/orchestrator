import OrchKit
import Testing

struct PRReviewSetupTests {
    @Test func readsUnknownValuesAsDefaults() {
        #expect(PRReviewSetup(instructionsFrom: "fork", compareAgainst: "head", projectHooks: "maybe") == .standard)
        #expect(PRReviewSetup(instructionsFrom: "", compareAgainst: "", projectHooks: "") == .standard)
        #expect(PRReviewSetup(instructionsFrom: "none", compareAgainst: "default", projectHooks: "off")
            == PRReviewSetup(instructionsFrom: .none, compareAgainst: .defaultBranch, projectHooks: .off))
    }

    @Test func widensOnlyTowardUnreviewedContent() {
        let standard = PRReviewSetup.standard
        #expect(PRReviewSetup(instructionsFrom: .base).widens(from: standard))
        #expect(PRReviewSetup(instructionsFrom: .head).widens(from: PRReviewSetup(instructionsFrom: .base)))
        #expect(PRReviewSetup(projectHooks: .on).widens(from: standard))
        #expect(!PRReviewSetup(instructionsFrom: .none).widens(from: standard))
        #expect(!PRReviewSetup(compareAgainst: .defaultBranch).widens(from: standard))
        #expect(!PRReviewSetup(projectHooks: .off).widens(from: PRReviewSetup(projectHooks: .on)))
        #expect(!PRReviewSetup(instructionsFrom: .head).widens(from: PRReviewSetup(instructionsFrom: .head)))
        #expect(!standard.widens(from: PRReviewSetup(instructionsFrom: .head, projectHooks: .on)))
    }

    @Test func warnsAboutWhatWidens() {
        #expect(PRReviewSetup.standard.warning.isEmpty)
        #expect(PRReviewSetup(instructionsFrom: .head).warning.contains("pr head"))
        #expect(PRReviewSetup(instructionsFrom: .head).warning.contains("stay off unless you turn them on"))
        #expect(!PRReviewSetup(instructionsFrom: .base, projectHooks: .on).warning.contains("stay off"))
        let both = PRReviewSetup(instructionsFrom: .base, projectHooks: .on).warning
        #expect(both.contains("pr base branch") && both.contains("hooks and MCP servers"))
    }
}
