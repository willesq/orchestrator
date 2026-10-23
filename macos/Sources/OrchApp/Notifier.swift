import AppKit
import OrchKit
import UserNotifications

/// Posts a notification for each PR that newly needs the user; clicking one
/// opens the PR.
@MainActor
final class Notifier: NSObject, UNUserNotificationCenterDelegate {
    private let center: UNUserNotificationCenter?

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
            content.body = "Open the Orchestrator menu to see them."
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

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse
    ) async {
        guard let raw = response.notification.request.content.userInfo["url"] as? String,
              let url = PullRequest(url: raw, title: "", repo: "").webURL
        else { return }
        await MainActor.run { _ = NSWorkspace.shared.open(url) }
    }

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter, willPresent notification: UNNotification
    ) async -> UNNotificationPresentationOptions {
        [.banner, .sound]
    }
}
