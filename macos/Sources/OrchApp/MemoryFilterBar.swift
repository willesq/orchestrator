import OrchKit
import SwiftUI

/// The filters Search and Browse share: a field that takes words and typed
/// `key:value` filters, labelled menus, and a removable chip per typed
/// filter. Menus and the field apply when the search is submitted; page size
/// and relevance only change what is shown.
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

    static let syntaxHelp = """
        Type filters into the field: type:repo_doc  repo:name  tag:owner:name  collection:name  \
        uri:session://…  scope:name  since:7d  until:2026-11-01. Ages: 24h, 7d, 2w, today, yesterday. \
        "Quoted phrases" stay part of the search. Several tag: or repo: filters must all match.
        """

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                TextField(prompt, text: $text)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit(submit)
                Image(systemName: "questionmark.circle")
                    .foregroundStyle(Color.secondaryText)
                    .help(Self.syntaxHelp)
                    .accessibilityLabel(Self.syntaxHelp)
                Button(submitTitle, action: submit)
                    .keyboardShortcut(.defaultAction)
            }
            FlowLayout(spacing: 14) {
                if let collections {
                    CollectionPicker(model: collections, selection: $collection).fixedSize()
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
                Picker(mode == nil ? "Page" : "Show", selection: $pageSize) {
                    ForEach(choices, id: \.self) { Text("\($0) at a time").tag($0) }
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
/// With spaces off there is one collection, so it is named, not offered.
struct CollectionPicker: View {
    let model: CollectionsModel
    @Binding var selection: String

    var body: some View {
        if let error = model.lastError, !model.loaded {
            Label("Collections unavailable", systemImage: "exclamationmark.triangle")
                .font(.caption)
                .foregroundStyle(.orange)
                .help(displaySafe(error))
        } else if !model.spacesOn && model.collections.count <= 1 {
            HStack(spacing: 6) {
                if let only = model.collections.first {
                    Text(verbatim: only.label(spacesOn: false)).font(.caption)
                }
                Text("Spaces are off: one collection").textStyle(.caption)
            }
            .help("With spaces on, each space has its own collection to choose from")
        } else {
            Picker("Collection", selection: $selection) {
                Text("All collections").tag("")
                Divider()
                ForEach(names, id: \.self) { Text(verbatim: model.label(for: $0)).tag($0) }
            }
            .help("Only results stored in this collection")
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
