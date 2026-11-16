import OrchKit

// The Memory pane's Browse tab, collection list and whole-source reader,
// wired the SP-081 way: FeatureStore models over the shared memory and
// catalog APIs, handed to the window through the SwiftUI environment.

extension AppModel {
    /// Browse: every chunk matching the filters, page by page.
    var memoryBrowse: BrowseModel { features.get { [client] in BrowseModel(api: client.core) } }

    /// The collections the filter menus offer, loaded once.
    var memoryCollections: CollectionsModel { features.get { [client] in CollectionsModel(api: client.core) } }

    /// A hit's whole source, rebuilt from its chunks.
    var sourceDocument: SourceDocumentModel { features.get { [client] in SourceDocumentModel(api: client.core) } }
}
