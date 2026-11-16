import AppKit
import OrchKit
import SwiftUI

/// A source rebuilt from memory: a note on how close it is to the original,
/// then its text. Shared by the sheet and the window it opens into.
struct SourceDocumentBody: View {
    let document: RebuiltDocument
    @State private var text: NSAttributedString?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Text(document.malformed
                ? "Some chunks were out of order, so this is a best effort."
                : "Close to the original but not exact: a table or code block split across chunks may look slightly off.")
                .textStyle(.caption)
                .padding(12)
            if document.oversize {
                Text("This source is too large to show here.").foregroundStyle(Color.secondaryText).padding(12)
                Spacer()
            } else if let text {
                RichTextView(text: text)
            } else {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .task(id: document.sourceURI) {
            text = nil
            let raw = document.text
            // Stored text is shown as its source, never rendered; escaping
            // control characters in a long source runs off the main thread.
            let safe = await Task.detached { displaySafeLines(raw) }.value
            guard !Task.isCancelled else { return }
            text = plainSource(safe)
        }
    }
}

/// A rebuilt source in a window of its own, which can be resized, zoomed
/// and taken full screen like any other: a long session transcript does
/// not fit the sheet it is first shown in.
private struct SourceDocumentWindowContent: View {
    let document: RebuiltDocument

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text(verbatim: displaySafe(document.sourceURI)).textStyle(.codeCaption).lineLimit(1).truncationMode(.middle)
                    .textSelection(.enabled)
                Spacer()
                Text("\(document.chunks) chunk\(document.chunks == 1 ? "" : "s")").textStyle(.caption)
            }
            .padding(12)
            Divider()
            SourceDocumentBody(document: document)
        }
        .frame(minWidth: 480, minHeight: 320)
    }
}

/// The open source windows, one per source: opening a source already shown
/// brings its window forward instead of adding another.
@MainActor
final class SourceDocumentWindows: NSObject, NSWindowDelegate {
    static let shared = SourceDocumentWindows()

    private var windows: [String: NSWindow] = [:]

    func open(_ document: RebuiltDocument) {
        if let window = windows[document.sourceURI] {
            window.makeKeyAndOrderFront(nil)
            return
        }
        let hosting = NSHostingController(rootView: SourceDocumentWindowContent(document: document))
        hosting.sizingOptions = []
        let window = NSWindow(contentViewController: hosting)
        window.title = displaySafe(document.sourceURI)
        window.styleMask.insert([.resizable, .miniaturizable, .closable, .titled])
        window.collectionBehavior.insert(.fullScreenPrimary)
        window.isReleasedWhenClosed = false
        window.setContentSize(NSSize(width: 900, height: 700))
        // One saved size for every source window, so a size chosen once sticks.
        if !window.setFrameUsingName("OrchSourceDocument") { window.center() }
        window.setFrameAutosaveName("OrchSourceDocument")
        window.delegate = self
        windows[document.sourceURI] = window
        window.makeKeyAndOrderFront(nil)
    }

    func windowWillClose(_ notification: Notification) {
        guard let closing = notification.object as? NSWindow else { return }
        windows = windows.filter { $0.value !== closing }
    }
}
