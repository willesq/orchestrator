import AppKit
import Foundation
import Testing
@testable import OrchKit

@Suite struct ClipboardTests {
    private func pasteboard() -> NSPasteboard {
        NSPasteboard(name: .init("ClipboardTests-\(UUID().uuidString)"))
    }

    @Test func copiesTheTextAsAPlainString() {
        let board = pasteboard()
        defer { board.releaseGlobally() }
        #expect(Clipboard.copy("orchd: connection refused", to: board))
        #expect(board.string(forType: .string) == "orchd: connection refused")
    }

    @Test func replacesWhatWasThere() {
        let board = pasteboard()
        defer { board.releaseGlobally() }
        board.clearContents()
        board.setString("old", forType: .string)
        board.setData(Data([1, 2]), forType: .png)
        Clipboard.copy("new error", to: board)
        #expect(board.string(forType: .string) == "new error")
        #expect(board.data(forType: .png) == nil)
    }

    @Test func keepsMultilineAndUnicodeIntact() {
        let board = pasteboard()
        defer { board.releaseGlobally() }
        let message = "first line\nsecond — line ✗"
        Clipboard.copy(message, to: board)
        #expect(board.string(forType: .string) == message)
    }
}
