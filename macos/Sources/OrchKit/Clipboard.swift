import AppKit

/// Puts text on a pasteboard, replacing what was there.
public enum Clipboard {
    /// Copies `text` as a plain string. The pasteboard is a parameter so
    /// tests can use a private one instead of the user's clipboard.
    @discardableResult
    public static func copy(_ text: String, to pasteboard: NSPasteboard = .general) -> Bool {
        pasteboard.clearContents()
        return pasteboard.setString(text, forType: .string)
    }
}
