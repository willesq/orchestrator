import OrchKit
import SwiftUI

/// The filters every Memory tab shares: a field that takes words and typed
/// `key:value` filters, labelled menus (Collection always among them), and a
/// removable chip per typed filter. The field applies when submitted; a
/// ranked tab's page size and relevance only change what is shown.
struct MemoryFilterBar: View {
    @Binding var text: String
    let prompt: String
    let submitTitle: String
    let submit: () -> Void
    let collections: CollectionsModel?
    @Binding var collection: String
    @Binding var sourceType: String
    let types: [String]
    /// Search's Match menu; nil for Browse, which does not rank.
    var mode: Binding<SearchMode>?
    @Binding var pageSize: Int
    let pageSizes: [Int]
    /// Search's relevance floor; nil for Browse.
    var minRelevance: Binding<Float>?
    let chips: [MemoryFilterChip]
    let notes: [String]
    let remove: (MemoryFilterChip) -> Void
    /// The `?` help: which typed filters this tab takes and what they do.
    var syntaxHelp = MemoryFilterSyntax.search
    /// The page-size menu's title; by default "Show" when ranked, else "Page".
    var pageTitle: String?
    /// Each page-size choice's label.
    var pageLabel: (Int) -> String = { "\($0) at a time" }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                TextField(prompt, text: $text)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit(submit)
                Image(systemName: "questionmark.circle")
                    .foregroundStyle(Color.secondaryText)
                    .help(syntaxHelp)
                    .accessibilityLabel(syntaxHelp)
                Button(submitTitle, action: submit)
                    .keyboardShortcut(.defaultAction)
            }
            FlowLayout(spacing: 14) {
                if let collections {
                    CollectionPicker(model: collections, selection: $collection)
                }
                Picker("Type", selection: $sourceType) {
                    Text("Any type").tag("")
                    Divider()
                    ForEach(typeChoices(known: types, current: sourceType), id: \.self) {
                        Text(verbatim: displaySafe($0)).tag($0)
                    }
                }
                .fixedSize()
                .help("Only results from sources of this type")
                if let mode {
                    Picker("Match", selection: mode) {
                        ForEach(SearchMode.allCases) { Text($0.label).tag($0).help($0.help) }
                    }
                    .fixedSize()
                    .help(mode.wrappedValue.help)
                }
                Picker(pageTitle ?? (mode == nil ? "Page" : "Show"), selection: $pageSize) {
                    ForEach(choices, id: \.self) { Text(pageLabel($0)).tag($0) }
                }
                .fixedSize()
                .help("How many results show before you ask for more")
                if let minRelevance {
                    Picker("Relevance", selection: minRelevance) {
                        ForEach(SearchModel.relevanceChoices, id: \.self) { value in
                            Text(value == 0 ? "Any" : String(format: "At least %.1f", value)).tag(value)
                        }
                    }
                    .fixedSize()
                    .help("Hide results scoring below this; the score's scale depends on how the search ranked")
                }
            }
            .controlSize(.small)
            if !chips.isEmpty {
                FlowLayout(spacing: 6) {
                    ForEach(chips) { chip in FilterChipView(chip: chip, remove: { remove(chip) }).fixedSize() }
                }
            }
            ForEach(notes, id: \.self) { note in
                Label { Text(verbatim: note) } icon: { Image(systemName: "info.circle") }
                    .font(.caption)
                    .foregroundStyle(.orange)
            }
        }
    }

    private var choices: [Int] {
        pageSizes.contains(pageSize) ? pageSizes : (pageSizes + [pageSize]).sorted()
    }
}

/// The `?` help for each Memory tab: every tab takes the same typed keys,
/// but words and `uri:` mean different things on each.
enum MemoryFilterSyntax {
    static let keys = "type:repo_doc  repo:name  tag:owner:name  collection:name  uri:session://…  scope:name  "
        + "since:7d  until:2026-11-01"
    static let common = "Ages: 24h, 7d, 2w, today, yesterday; until: runs to the end of that day. "
        + "Several tag: or repo: filters must all match. A typed type:, collection: or uri: replaces the menu's."

    static let search = "Type filters into the field: \(keys). Words are searched and ranked; "
        + "\"quoted phrases\" stay part of the search; uri: is one source. \(common)"
    static let browse = "Type filters into the field: \(keys). Browse lists by filters only, so words are ignored; "
        + "uri: is one source. \(common)"
    static let indexed = "Type filters into the field: \(keys). Indexed lists sources by filters only, so words are "
        + "ignored; uri: matches every source whose address starts with it, e.g. uri:session://. \(common)"
    static let directMessages = "Type filters into the field: \(keys). Words are searched and ranked among chat and "
        + "direct messages; the Type menu starts at chat, and Any type adds the rest of memory. uri: is one "
        + "conversation. \(common)"
}

/// One typed filter, with an x that removes it and searches again.
struct FilterChipView: View {
    let chip: MemoryFilterChip
    let remove: () -> Void

    var body: some View {
        HStack(spacing: 4) {
            Text(verbatim: chip.label)
            Button(action: remove) { Image(systemName: "xmark").imageScale(.small) }
                .buttonStyle(.plain)
                .help("Remove this filter")
                .accessibilityLabel("Remove \(chip.label)")
        }
        .font(.caption.weight(.medium))
        .padding(.horizontal, 7)
        .padding(.vertical, 2)
        .background(Color.accentColor.opacity(0.18), in: Capsule())
        .foregroundStyle(Color.accentColor)
    }
}

/// "All collections" and each readable collection with its chunk count.
/// Always a menu, whether spaces are on or off, so the filter stays in the
/// same place; with spaces off it offers the one collection there is.
struct CollectionPicker: View {
    let model: CollectionsModel
    @Binding var selection: String

    var body: some View {
        HStack(spacing: 6) {
            Picker("Collection", selection: $selection) {
                Text("All collections").tag("")
                if !names.isEmpty { Divider() }
                ForEach(names, id: \.self) { Text(verbatim: model.label(for: $0)).tag($0) }
            }
            .help("Only results stored in this collection")
            .fixedSize()
            if let error = model.lastError, !model.loaded {
                Image(systemName: "exclamationmark.triangle")
                    .foregroundStyle(.orange)
                    .help("Collections unavailable: \(displaySafe(error))")
                    .accessibilityLabel("Collections unavailable")
            } else if model.loaded && !model.spacesOn {
                // Reason: at the companion column's width the caption does
                // not fit beside the menu, so it gives way to an icon.
                ViewThatFits(in: .horizontal) {
                    Text("Spaces are off: one collection").textStyle(.caption).fixedSize()
                    Image(systemName: "info.circle")
                        .foregroundStyle(Color.secondaryText)
                        .accessibilityLabel("Spaces are off: one collection")
                }
                .help("Spaces are off: one collection. With spaces on, each space has its own collection to choose from")
            }
        }
    }

    /// The listed collections, plus a chosen one not listed.
    private var names: [String] {
        let listed = model.collections.map(\.name)
        return selection.isEmpty || listed.contains(selection) ? listed : listed + [selection]
    }
}

extension Binding where Value == [String] {
    /// One choice from a list filter: "" for none.
    var single: Binding<String> {
        Binding<String>(get: { wrappedValue.first ?? "" }, set: { wrappedValue = $0.isEmpty ? [] : [$0] })
    }
}
