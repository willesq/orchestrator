import AppKit
import Foundation
import Testing
@testable import OrchKit

@Suite struct MarkdownRendererTests {
    private let base = URL(string: "https://github.com/willesq/orchestrator/blob/abc123/docs/INSTALL.md")!

    private func render(_ md: String) throws -> NSAttributedString { try #require(MarkdownRenderer(baseURL: base).render(md)) }

    /// The attribute at the first occurrence of `text`.
    private func attribute(_ key: NSAttributedString.Key, of text: String, in s: NSAttributedString) -> Any? {
        let range = (s.string as NSString).range(of: text)
        guard range.location != NSNotFound else { return nil }
        return s.attribute(key, at: range.location, effectiveRange: nil)
    }

    private func font(of text: String, in s: NSAttributedString) -> NSFont? {
        attribute(.font, of: text, in: s) as? NSFont
    }

    @Test func headingsParagraphsAndInlineStyles() throws {
        let s = try render("# Install\n\nRun **make** and *wait*, ~~not~~ `brew`.\nNext line.")
        #expect(s.string == "Install\nRun make and wait, not brew. Next line.")
        let heading = try #require(font(of: "Install", in: s)), body = try #require(font(of: "Run", in: s))
        #expect(heading.pointSize > body.pointSize)
        // Font descriptors, not NSFontManager, which is not safe on a test's thread.
        #expect(try #require(font(of: "make", in: s)).fontDescriptor.symbolicTraits.contains(.bold))
        #expect(try #require(font(of: "wait", in: s)).fontDescriptor.symbolicTraits.contains(.italic))
        #expect(attribute(.strikethroughStyle, of: "not", in: s) as? Int == NSUnderlineStyle.single.rawValue)
        #expect(try #require(font(of: "brew", in: s)).isFixedPitch)
    }

    @Test func listsQuotesCodeAndRules() throws {
        let s = try render("""
        - one
        - two
          1. nested
        - [x] done

        > quoted

        ```go
        fmt.Println("hi")
        ```

        ---
        """)
        #expect(s.string.contains("•\tone"))
        #expect(s.string.contains("1.\tnested"))
        #expect(s.string.contains("☑\tdone"))
        #expect(s.string.contains("fmt.Println(\"hi\")"))
        #expect(try #require(font(of: "fmt.Println", in: s)).isFixedPitch)
        #expect(attribute(.foregroundColor, of: "quoted", in: s) as? NSColor == NSColor.secondaryLabelColor)
        #expect(s.string.contains("────"))
        let nestedIndent = (attribute(.paragraphStyle, of: "nested", in: s) as? NSParagraphStyle)?.headIndent ?? 0
        let topIndent = (attribute(.paragraphStyle, of: "one", in: s) as? NSParagraphStyle)?.headIndent ?? 0
        #expect(nestedIndent > topIndent)
    }

    @Test func tablesBecomeAlignedRows() throws {
        let s = try render("| Name | Size |\n|---|---|\n| orch.db | 140 KB |\n| qdrant | 1.7 GB |")
        #expect(s.string.contains("Name    │ Size"))
        #expect(s.string.contains("orch.db │ 140 KB"))
        #expect(try #require(font(of: "Name", in: s)).isFixedPitch)
    }

    @Test func linksResolveAgainstTheDocAndOnlyHTTPOpens() throws {
        let s = try render("""
        [abs](https://go.dev/doc) [rel](OPERATIONS.md) [up](../README.md#install) \
        [js](javascript:alert(1)) [file](file:///etc/passwd) [mail](mailto:a@b.c) [creds](https://u:p@x.dev)
        """)
        #expect((attribute(.link, of: "abs", in: s) as? URL)?.absoluteString == "https://go.dev/doc")
        #expect((attribute(.link, of: "rel", in: s) as? URL)?.absoluteString
            == "https://github.com/willesq/orchestrator/blob/abc123/docs/OPERATIONS.md")
        #expect((attribute(.link, of: "up", in: s) as? URL)?.absoluteString
            == "https://github.com/willesq/orchestrator/blob/abc123/README.md#install")
        for text in ["js", "file", "mail", "creds"] {
            #expect(attribute(.link, of: text, in: s) == nil, "\(text) must not be a link")
        }
        // Without a base, a relative link goes nowhere.
        #expect(try #require(MarkdownRenderer(baseURL: nil).render("[rel](x.md)")).attribute(.link, at: 0, effectiveRange: nil) == nil)
        #expect(MarkdownRenderer.linkURL("  ", base: base) == nil)
    }

    @Test func imagesAreNeverLoadedAndHTMLIsShownAsText() throws {
        let s = try render("![diagram of orchd](https://x.dev/track.png) and ![](y.png)\n\n<script>alert(1)</script>\n\nInline <b>bold</b>.")
        #expect(s.string.contains("[image: diagram of orchd]"))
        #expect(s.string.contains("[image]"))
        #expect(s.string.contains("<script>alert(1)</script>"))
        #expect(s.string.contains("<b>bold</b>"))
        var attachments = 0
        s.enumerateAttribute(.attachment, in: NSRange(location: 0, length: s.length)) { value, _, _ in
            if value != nil { attachments += 1 }
        }
        #expect(attachments == 0)
        #expect(attribute(.link, of: "diagram", in: s) == nil)
    }

    @Test func pathologicalNestingFallsBackToSource() async {
        // Both crashed the app's parser on a worker thread's stack.
        let quotes = String(repeating: ">", count: 700) + " x"
        let emphasis = String(repeating: "*", count: 10_000) + "x" + String(repeating: "*", count: 10_000)
        #expect(MarkdownRenderer.tooDeep(quotes) && MarkdownRenderer.tooDeep(emphasis))
        #expect(MarkdownRenderer().render(quotes) == nil)
        #expect(await MarkdownRenderer().renderInBackground(emphasis) == nil)
        #expect(!MarkdownRenderer.tooDeep("> > quoted\n**bold** `code` [link](x)"))
        // Just under the pre-scan, on the large stack: renders, depth capped.
        let deep = String(repeating: ">", count: 60) + " deep"
        let made = await MarkdownRenderer().renderInBackground(deep)
        #expect(made?.text.string.contains("deep") == true)
    }

    @Test func cancellingStopsARender() async {
        let big = String(repeating: "- item **bold** and `code`\n", count: 20_000)
        let task = Task { await MarkdownRenderer().renderInBackground(big) }
        task.cancel()
        #expect(await task.value == nil)
    }

    @Test func tablesStayInProportionToTheirInput() throws {
        var md = "| a | b |\n|---|---|\n| " + String(repeating: "x", count: 20_000) + " | y |\n"
        md += String(repeating: "| s | t |\n", count: 2_000)
        let s = try render(md)
        // Padding is bounded by the table cap, not by rows times the widest cell
        // (which was 40 MB for this input).
        #expect(s.length <= MarkdownRenderer.maxTableChars + md.count, "rendered \(s.length) characters from \(md.count)")
        // Past the cap, cells go unpadded: output stays in proportion to input.
        let wide = "| a | b |\n|---|---|\n" + String(repeating: "| " + String(repeating: "w", count: 70) + " | v |\n", count: 5_000)
        #expect(try render(wide).length < wide.count * 2)
        #expect(s.string.contains("…"))
    }

    @Test func linkTextNamingAnotherHostShowsTheRealOne() throws {
        let s = try render("[https://github.com/org/repo](https://evil.example/x) and [docs](https://go.dev)")
        #expect(s.string.contains("https://github.com/org/repo (evil.example)"))
        #expect(!s.string.contains("docs (go.dev)"))
    }

    @Test func lineBreaksAndEmptyInput() throws {
        #expect(try render("a  \nb").string == "a\u{2028}b")
        #expect(try render("").string.isEmpty)
        #expect(try render("-").string.contains("•"))
    }

    @Test func markdownDetection() {
        #expect(RebuiltDocument(sourceURI: "repo://o/r/x.txt", sourceType: "repo_doc", text: "").isMarkdown)
        #expect(RebuiltDocument(sourceURI: "manual://notes/plan.md", text: "").isMarkdown)
        #expect(RebuiltDocument(sourceURI: "https://x.dev/README.markdown", sourceType: "web", text: "").isMarkdown)
        #expect(!RebuiltDocument(sourceURI: "https://go.dev/doc", sourceType: "web", text: "").isMarkdown)
        #expect(!RebuiltDocument(sourceURI: "session://abc/transcript", sourceType: "session_transcript", text: "").isMarkdown)
    }
}
