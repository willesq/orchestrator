import Testing
@testable import OrchKit

@MainActor
private final class First {
    let id: Int
    init(id: Int) { self.id = id }
}

@MainActor
private final class Second {
    let first: First
    init(first: First) { self.first = first }
}

@MainActor
struct FeatureStoreTests {
    /// A feature is built on first use and the same instance is returned
    /// after, without building it again.
    @Test func buildsOnceAndKeepsTheFeature() {
        let store = FeatureStore()
        var builds = 0
        let a = store.get { builds += 1; return First(id: 1) }
        let b = store.get { builds += 1; return First(id: 2) }
        #expect(a === b)
        #expect(a.id == 1)
        #expect(builds == 1)
    }

    /// Different feature types are kept apart, and one feature's build can
    /// ask for another, which it then shares with everyone else.
    @Test func featuresAreKeptByTypeAndMayDependOnEachOther() {
        let store = FeatureStore()
        let second = store.get(Second.self) { Second(first: store.get { First(id: 7) }) }
        let first = store.get(First.self) { First(id: 8) }
        #expect(second.first === first)
        #expect(first.id == 7)
    }

    /// Two stores hold separate features, as two app models would.
    @Test func storesDoNotShareFeatures() {
        let one = FeatureStore(), two = FeatureStore()
        let a = one.get { First(id: 1) }
        let b = two.get { First(id: 2) }
        #expect(a !== b)
    }
}
