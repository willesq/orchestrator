import AppKit
import OrchKit
import UserNotifications

/// Posts a notification for each PR that newly needs the user, each new
/// review verdict and each tool call the spaces guard refused; clicking a PR's
/// shows that PR in the app's PRs pane, a refusal's the Spaces settings.
@MainActor
final class Notifier: NSObject, UNUserNotificationCenterDelegate {
    private let center: UNUserNotificationCenter?

    /// Shows a PR in the app; given only exact github.com pull URLs.
    var onOpenPR: (@MainActor (String) -> Void)?
    /// Shows the PRs that need the user, for the summary notification.
    var onOpenNeedsYou: (@MainActor () -> Void)?
    /// Shows where refusals are explained, for a refusal notification.
    var onOpenRefusals: (@MainActor () -> Void)?

    override init() {
        // UNUserNotificationCenter traps when the binary runs outside an app
        // bundle (e.g. `swift run`), so notifications only exist in Orch.app.
        center = Bundle.main.bundleIdentifier == nil ? nil : UNUserNotificationCenter.current()
        super.init()
        center?.delegate = self
        center?.requestAuthorization(options: [.alert, .sound]) { _, _ in }
    }

    func post(_ prs: [PullRequest]) {
        guard let center else { return }
        switch NotificationPlan(prs) {
        case .summary(let count):
            let content = UNMutableNotificationContent()
            content.title = "\(count) PRs need you"
            content.body = "Click to see them in Orchestrator."
            content.userInfo = ["open": "needs-you"]
            center.add(UNNotificationRequest(identifier: "summary", content: content, trigger: nil))
        case .each(let prs):
            postEach(prs, to: center)
        }
    }

    private func postEach(_ prs: [PullRequest], to center: UNUserNotificationCenter) {
        for pr in prs {
            guard let attention = pr.attention, let url = pr.webURL else { continue }
            let content = UNMutableNotificationContent()
            content.title = attention.label
            content.subtitle = pr.repo
            content.body = pr.title
            content.threadIdentifier = pr.repo
            content.userInfo = ["url": url.absoluteString]
            // One per PR and reason, so a repeat replaces rather than stacks.
            let request = UNNotificationRequest(
                identifier: "\(pr.url)|\(attention.rawValue)", content: content, trigger: nil
            )
            center.add(request)
        }
    }

    /// One notification per new verdict; the reason is the body, cut short.
    func post(verdicts: [PRReview]) {
        guard let center else { return }
        for review in verdicts {
            guard let verdict = review.verdict, let url = PullRequest(url: review.url, title: "", repo: "").webURL else { continue }
            let content = UNMutableNotificationContent()
            content.title = "Review verdict: \(verdict.label)"
            content.subtitle = "\(review.repo)#\(review.number)"
            content.body = String(displaySafeLines(review.reason).prefix(240))
            content.threadIdentifier = "reviews"
            content.userInfo = ["url": url.absoluteString]
            // One per PR, so a newer verdict replaces the last.
            center.add(UNNotificationRequest(identifier: "verdict|\(review.url)", content: content, trigger: nil))
        }
    }

    /// One notification per PR newly held for the user's go-ahead.
    func post(held: [PRReview]) {
        guard let center else { return }
        for review in held {
            guard let url = PullRequest(url: review.url, title: "", repo: "").webURL else { continue }
            let content = UNMutableNotificationContent()
            content.title = "Review waiting for your go-ahead"
            content.subtitle = "\(review.repo)#\(review.number)"
            content.body = "Its author is not a member or collaborator, so orch does not run their code until you look."
            content.threadIdentifier = "reviews"
            content.userInfo = ["url": url.absoluteString]
            center.add(UNNotificationRequest(identifier: "held|\(review.url)", content: content, trigger: nil))
        }
    }

    /// One notification per refused tool call, or one summary for a burst
    /// (SP-048). It names the space, tool, rule and session, and a term only
    /// by its space, line and hash; the refusal never carries the call's text.
    func post(refusals: [Refusal]) {
        guard let center else { return }
        switch RefusalNotificationPlan(refusals) {
        case .summary(let count, let spaces):
            let content = UNMutableNotificationContent()
            content.title = "\(count) tool calls refused"
            content.body = String(displaySafe("In \(spaces.joined(separator: ", ")). See them with 'orch spaces log'.").prefix(240))
            content.threadIdentifier = "refusals"
            content.userInfo = ["open": "refusals"]
            // One summary at a time: a later burst replaces it.
            center.add(UNNotificationRequest(identifier: "refusals-summary", content: content, trigger: nil))
        case .each(let refusals):
            for refusal in refusals {
                let content = UNMutableNotificationContent()
                content.title = "Tool call refused"
                content.subtitle = displaySafe(refusal.place)
                content.body = displaySafe(refusal.detail)
                content.threadIdentifier = "refusals"
                content.userInfo = ["open": "refusals"]
                center.add(UNNotificationRequest(identifier: "refusal|\(UUID().uuidString)", content: content, trigger: nil))
            }
        }
    }

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse
    ) async {
        let info = response.notification.request.content.userInfo
        if info["open"] as? String == "needs-you" {
            await MainActor.run { onOpenNeedsYou?() }
            return
        }
        if info["open"] as? String == "refusals" {
            await MainActor.run { onOpenRefusals?() }
            return
        }
        guard let raw = info["url"] as? String,
              let url = PullRequest(url: raw, title: "", repo: "").webURL
        else { return }
        await MainActor.run {
            if let onOpenPR {
                onOpenPR(url.absoluteString)
            } else {
                _ = NSWorkspace.shared.open(url)
            }
        }
    }

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter, willPresent notification: UNNotification
    ) async -> UNNotificationPresentationOptions {
        [.banner, .sound]
    }
}
