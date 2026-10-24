import AppKit
import Markdown

/// Renders a stored markdown doc for reading. The doc is indexed content (third-
/// party repos, agent-written memories), so rendering is deliberately inert:
/// images are never loaded (their alt text is shown), raw HTML is shown as
/// text rather than interpreted, and only plain http(s) links are made
/// clickable, with relative ones resolved against the doc's GitHub location.
public struct MarkdownRenderer: Sendable {
    /// The doc's own URL, against which relative links resolve.
    public var baseURL: URL?
    public var fontSize: CGFloat

    public init(baseURL: URL? = nil, fontSize: CGFloat = NSFont.systemFontSize) {
        self.baseURL = baseURL
        self.fontSize = fontSize
    }

    /// Renders `source`, or returns nil when it is nested too deeply to
    /// render safely (the caller shows the source instead) or when cancelled.
    /// The parser recurses once per level of nesting, so call this on a thread
    /// with a large stack: `renderInBackground` does.
    public func render(_ source: String, isCancelled: @escaping @Sendable () -> Bool = { false }) -> NSAttributedString? {
        guard !Self.tooDeep(source) else { return nil }
        let builder = Builder(base: baseURL, size: fontSize, isCancelled: isCancelled)
        builder.block(Document(parsing: source, options: [.disableSmartOpts]))
        guard !isCancelled() else { return nil }
        return builder.result()
    }

    /// Renders on a dedicated thread with a large stack, so deeply nested input
    /// cannot overflow a concurrency worker's small one; cancelling the caller
    /// stops the render.
    public func renderInBackground(_ source: String) async -> RenderedText? {
        let flag = CancelFlag()
        return await withTaskCancellationHandler {
            await withCheckedContinuation { (continuation: CheckedContinuation<RenderedText?, Never>) in
                let renderer = self
                let thread = Thread {
                    let text = renderer.render(source, isCancelled: { flag.isSet })
                    continuation.resume(returning: text.map(RenderedText.init))
                }
                thread.stackSize = 64 << 20
                thread.qualityOfService = .userInitiated
                thread.start()
            }
        } onCancel: {
            flag.set()
        }
    }

    /// Nesting past what is worth rendering: quote markers deeper than
    /// `maxQuoteDepth` at the start of a line, or a run of emphasis, link or
    /// code markers longer than `maxMarkerRun`. Linear in the input.
    public static func tooDeep(_ source: String) -> Bool {
        var atLineStart = true, quotes = 0
        var runChar: Character?, run = 0
        for ch in source {
            if ch == "\n" {
                atLineStart = true
                quotes = 0
            } else if atLineStart {
                if ch == ">" {
                    quotes += 1
                    if quotes > maxQuoteDepth { return true }
                } else if ch != " " && ch != "\t" {
                    atLineStart = false
                }
            }
            if "*_~[`".contains(ch) {
                run = ch == runChar ? run + 1 : 1
                runChar = ch
                if run > maxMarkerRun { return true }
            } else {
                runChar = nil
                run = 0
            }
        }
        return false
    }

    static let maxQuoteDepth = 64
    static let maxMarkerRun = 256
    /// Past this depth, nested content is shown as its plain text.
    static let maxRenderDepth = 32
    /// Table cells are cut to this many characters.
    static let maxCellWidth = 80
    /// A table whose padded rows would pass this many characters is shown
    /// unpadded instead.
    static let maxTableChars = 256 * 1024

    /// Where a link may go: a plain http(s) URL, with a relative destination
    /// resolved against `base`; anything else is not a link.
    public static func linkURL(_ destination: String, base: URL?) -> URL? {
        let trimmed = destination.trimmingCharacters(in: .whitespaces)
        guard !trimmed.isEmpty, let parsed = URL(string: trimmed, relativeTo: base)?.absoluteURL,
              let scheme = parsed.scheme?.lowercased(), scheme == "http" || scheme == "https",
              let host = parsed.host, !host.isEmpty, parsed.user == nil, parsed.password == nil
        else { return nil }
        return parsed
    }
}

/// A rendered doc, built on a background thread and not changed afterwards
/// (it is an immutable copy), so handing it to the main actor shares nothing
/// mutable.
public struct RenderedText: @unchecked Sendable {
    public let text: NSAttributedString
}

/// A one-way cancellation flag shared with the rendering thread.
private final class CancelFlag: @unchecked Sendable {
    private let lock = NSLock()
    private var value = false
    func set() { lock.withLock { value = true } }
    var isSet: Bool { lock.withLock { value } }
}

/// The inline style in effect while walking a block's children.
private struct Style {
    var bold = false
    var italic = false
    var mono = false
    var strike = false
    var secondary = false
    var size: CGFloat
    var link: URL?
}

private final class Builder {
    private let out = NSMutableAttributedString()
    private let base: URL?
    private let size: CGFloat
    private var listDepth = 0
    private var quoteDepth = 0
    /// A list marker the next block starts with.
    private var pendingMarker: String?
    private let isCancelled: () -> Bool
    /// How deeply nested the block or inline being rendered is.
    private var depth = 0

    init(base: URL?, size: CGFloat, isCancelled: @escaping () -> Bool) {
        self.base = base
        self.size = size
        self.isCancelled = isCancelled
    }

    func result() -> NSAttributedString {
        // No trailing paragraph break after the last block.
        while out.string.hasSuffix("\n") {
            out.deleteCharacters(in: NSRange(location: out.length - 1, length: 1))
        }
        // A copy, so what is handed on is immutable.
        return out.copy() as! NSAttributedString
    }

    // MARK: Blocks

    func block(_ markup: Markup) {
        if isCancelled() { return }
        depth += 1
        defer { depth -= 1 }
        // Deeper than this, nesting adds nothing worth the recursion.
        if depth > MarkdownRenderer.maxRenderDepth {
            append(NSAttributedString(string: markup.format(), attributes: attributes(body())))
            return
        }
        switch markup {
        case let heading as Heading:
            let scale: [CGFloat] = [1.7, 1.45, 1.25, 1.1, 1.0, 0.95]
            let style = Style(bold: true, size: size * scale[min(max(heading.level, 1), 6) - 1])
            append(inlines(heading, style), spacingBefore: size * 0.9, spacingAfter: size * 0.35)
        case let paragraph as Paragraph:
            append(inlines(paragraph, body()))
        case let code as CodeBlock:
            var text = code.code
            if text.hasSuffix("\n") { text.removeLast() }
            let attrs = attributes(Style(mono: true, size: size * 0.92))
                .merging([.backgroundColor: NSColor.quaternaryLabelColor.withAlphaComponent(0.12)]) { $1 }
            append(NSAttributedString(string: text, attributes: attrs), extraIndent: 12)
        case let quote as BlockQuote:
            quoteDepth += 1
            quote.children.forEach(block)
            quoteDepth -= 1
        case let list as UnorderedList:
            listDepth += 1
            for item in list.listItems {
                let marker = item.checkbox.map { $0 == .checked ? "☑" : "☐" } ?? (listDepth.isMultiple(of: 2) ? "◦" : "•")
                listItem(item, marker: marker)
            }
            listDepth -= 1
        case let list as OrderedList:
            listDepth += 1
            for (offset, item) in list.listItems.enumerated() {
                listItem(item, marker: "\(Int(list.startIndex) + offset).")
            }
            listDepth -= 1
        case is ThematicBreak:
            append(NSAttributedString(string: String(repeating: "─", count: 24),
                                      attributes: attributes(Style(secondary: true, size: size))))
        case let table as Table:
            self.table(table)
        case let html as HTMLBlock:
            // Shown as the source it is, never interpreted.
            var raw = html.rawHTML
            if raw.hasSuffix("\n") { raw.removeLast() }
            append(NSAttributedString(string: raw, attributes: attributes(Style(mono: true, secondary: true, size: size * 0.92))))
        default:
            markup.children.forEach(block)
        }
    }

    private func listItem(_ item: ListItem, marker: String) {
        pendingMarker = marker
        if item.childCount == 0 {
            append(NSAttributedString(string: "", attributes: attributes(body())))
        }
        item.children.forEach(block)
        pendingMarker = nil
    }

    /// A table as aligned monospaced rows: the header bold, columns padded.
    /// Cells are cut to a width, and a table too large to pad is shown with
    /// its cells unpadded, so the output stays in proportion to the input.
    private func table(_ table: Table) {
        let cut = { (s: String) in
            s.count > MarkdownRenderer.maxCellWidth ? String(s.prefix(MarkdownRenderer.maxCellWidth - 1)) + "…" : s
        }
        var rows: [[String]] = [table.head.cells.map { cut($0.plainText) }]
        rows += table.body.rows.map { $0.cells.map { cut($0.plainText) } }
        let columns = rows.map(\.count).max() ?? 0
        var widths = Array(repeating: 0, count: columns)
        for row in rows {
            for (i, cell) in row.enumerated() { widths[i] = max(widths[i], cell.count) }
        }
        if rows.count * (widths.reduce(0, +) + 3 * columns) > MarkdownRenderer.maxTableChars {
            widths = Array(repeating: 0, count: columns)
        }
        let lines = NSMutableAttributedString()
        for (r, row) in rows.enumerated() {
            let padded = (0..<columns).map { i in
                let cell = i < row.count ? row[i] : ""
                return cell.padding(toLength: widths[i], withPad: " ", startingAt: 0)
            }
            let style = Style(bold: r == 0, mono: true, size: size * 0.92)
            lines.append(NSAttributedString(string: padded.joined(separator: " │ "), attributes: attributes(style)))
            if r == 0 {
                let rule = widths.map { String(repeating: "─", count: $0) }.joined(separator: "─┼─")
                lines.append(NSAttributedString(string: "\n" + rule, attributes: attributes(Style(mono: true, secondary: true, size: size * 0.92))))
            }
            if r < rows.count - 1 { lines.append(NSAttributedString(string: "\n")) }
        }
        append(lines)
    }

    /// Appends one block as its own paragraph, indented for the lists and
    /// quotes it sits in, and starting with a pending list marker if any.
    private func append(_ content: NSAttributedString, spacingBefore: CGFloat = 0, spacingAfter: CGFloat? = nil,
                        extraIndent: CGFloat = 0) {
        let line = NSMutableAttributedString()
        let indent = CGFloat(listDepth) * 18 + CGFloat(quoteDepth) * 16 + extraIndent
        let paragraph = NSMutableParagraphStyle()
        paragraph.paragraphSpacingBefore = spacingBefore
        paragraph.paragraphSpacing = spacingAfter ?? size * 0.6
        paragraph.headIndent = indent
        paragraph.firstLineHeadIndent = indent
        if let marker = pendingMarker {
            pendingMarker = nil
            paragraph.firstLineHeadIndent = max(indent - 14, 0)
            paragraph.tabStops = [NSTextTab(textAlignment: .left, location: indent)]
            line.append(NSAttributedString(string: marker + "\t", attributes: attributes(body())))
        }
        line.append(content)
        if quoteDepth > 0 {
            line.addAttribute(.foregroundColor, value: NSColor.secondaryLabelColor, range: NSRange(location: 0, length: line.length))
        }
        line.append(NSAttributedString(string: "\n"))
        line.addAttribute(.paragraphStyle, value: paragraph, range: NSRange(location: 0, length: line.length))
        out.append(line)
    }

    // MARK: Inlines

    private func body() -> Style { Style(size: size) }

    private func inlines(_ markup: Markup, _ style: Style) -> NSAttributedString {
        let result = NSMutableAttributedString()
        for child in markup.children { result.append(inline(child, style)) }
        return result
    }

    private func inline(_ markup: Markup, _ style: Style) -> NSAttributedString {
        depth += 1
        defer { depth -= 1 }
        if depth > MarkdownRenderer.maxRenderDepth {
            return NSAttributedString(string: markup.format(), attributes: attributes(style))
        }
        var style = style
        switch markup {
        case let text as Markdown.Text:
            return NSAttributedString(string: text.string, attributes: attributes(style))
        case is Emphasis:
            style.italic = true
        case is Strong:
            style.bold = true
        case is Strikethrough:
            style.strike = true
        case let code as InlineCode:
            style.mono = true
            return NSAttributedString(string: code.code, attributes: attributes(style)
                .merging([.backgroundColor: NSColor.quaternaryLabelColor.withAlphaComponent(0.12)]) { $1 })
        case let link as Markdown.Link:
            if let destination = link.destination, let url = MarkdownRenderer.linkURL(destination, base: base) {
                style.link = url
                // Text that reads as a URL elsewhere must not pass for the
                // destination: say where the link really goes.
                let shown = inlines(link, style)
                if let named = URL(string: link.plainText.trimmingCharacters(in: .whitespaces)), let namedHost = named.host,
                   namedHost.lowercased() != url.host?.lowercased() {
                    var note = style
                    note.link = nil
                    note.secondary = true
                    let out = NSMutableAttributedString(attributedString: shown)
                    out.append(NSAttributedString(string: " (\(url.host ?? ""))", attributes: attributes(note)))
                    return out
                }
                return shown
            }
        case let image as Markdown.Image:
            // Never fetched: the alt text stands in for it.
            let alt = image.plainText.isEmpty ? "image" : "image: \(image.plainText)"
            style.secondary = true
            style.italic = true
            return NSAttributedString(string: "[\(alt)]", attributes: attributes(style))
        case let html as InlineHTML:
            style.mono = true
            style.secondary = true
            return NSAttributedString(string: html.rawHTML, attributes: attributes(style))
        case is SoftBreak:
            return NSAttributedString(string: " ", attributes: attributes(style))
        case is LineBreak:
            return NSAttributedString(string: "\u{2028}", attributes: attributes(style))
        default:
            break
        }
        return inlines(markup, style)
    }

    private func attributes(_ style: Style) -> [NSAttributedString.Key: Any] {
        var font = style.mono
            ? NSFont.monospacedSystemFont(ofSize: style.size, weight: style.bold ? .semibold : .regular)
            : NSFont.systemFont(ofSize: style.size, weight: style.bold ? .semibold : .regular)
        if style.italic {
            // A descriptor rather than NSFontManager, which is not safe off the
            // main thread.
            let descriptor = font.fontDescriptor.withSymbolicTraits(font.fontDescriptor.symbolicTraits.union(.italic))
            font = NSFont(descriptor: descriptor, size: style.size) ?? font
        }
        var attrs: [NSAttributedString.Key: Any] = [
            .font: font,
            .foregroundColor: style.secondary ? NSColor.secondaryLabelColor : NSColor.labelColor,
        ]
        if style.strike { attrs[.strikethroughStyle] = NSUnderlineStyle.single.rawValue }
        if let url = style.link {
            attrs[.link] = url
            attrs[.toolTip] = url.absoluteString
        }
        return attrs
    }
}
