import LocalAuthentication
import OrchKit
import SwiftUI
import UserNotifications

/// The operator unlocks in the menu bar panel (SP-077): each pending request
/// with the command and terminal that asked and how long it asks for, to
/// confirm or deny; each confirmed one with the code to type in the
/// terminal; and each active unlock with its countdown, to lock.
struct UnlocksSection: View {
    let model: UnlocksModel

    var body: some View {
        if !model.unlocks.isEmpty {
            VStack(alignment: .leading, spacing: 8) {
                Text("Operator unlocks").font(.subheadline.weight(.semibold))
                ForEach(model.unlocks) { UnlockRow(unlock: $0, model: model) }
            }
        }
    }
}

struct UnlockRow: View {
    let unlock: OperatorUnlock
    let model: UnlocksModel

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Image(systemName: symbol)
                    .foregroundStyle(unlock.isActive ? Color.green : Color.orange)
                    .accessibilityHidden(true)
                Text(verbatim: title)
                    .font(.callout.weight(.medium))
                Spacer(minLength: 0)
                if !unlock.isActive {
                    Text(verbatim: "for \(unlock.durationText)").font(.caption).foregroundStyle(Color.secondaryText)
                } else {
                    TimelineView(.periodic(from: .now, by: 1)) { context in
                        Text(verbatim: "\(unlock.countdown(at: context.date)) left")
                            .font(.caption.monospacedDigit())
                            .foregroundStyle(Color.secondaryText)
                    }
                }
            }
            if unlock.state == .confirmed {
                VStack(alignment: .leading, spacing: 2) {
                    Text(verbatim: "Type this code in the terminal that asked (\(unlock.terminalText)):").font(.caption)
                    Text(verbatim: unlock.code).font(.title2.monospacedDigit().weight(.semibold)).textSelection(.enabled)
                }
                .help("Nothing is granted until the code is typed there. If no terminal of yours is waiting for a code, lock it.")
            }
            Text(verbatim: unlock.command)
                .font(.caption.monospaced())
                .lineLimit(3)
                .textSelection(.enabled)
            Text(verbatim: "\(unlock.terminalText) · process \(unlock.pid)\(unlock.executable.isEmpty ? "" : " (\(unlock.executable))") · \(unlock.origin)")
                .font(.caption2)
                .foregroundStyle(Color.secondaryText)
                .lineLimit(2)
                .help(unlock.origin)
            if unlock.isPending {
                Text("Confirming lets this terminal do anything Orch.app can, including changing the spaces policy, for \(unlock.durationText).")
                    .font(.caption2)
                    .foregroundStyle(Color.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let error = model.errors[unlock.id] {
                Text(verbatim: error).font(.caption).foregroundStyle(.red).fixedSize(horizontal: false, vertical: true)
            }
            HStack {
                Spacer()
                if model.busy.contains(unlock.id) { ProgressView().controlSize(.small) }
                if unlock.isPending {
                    Button("Deny") { Task { await model.deny(unlock) } }
                    Button("Confirm…") { Task { await model.confirm(unlock) } }
                        .help("Asks for Touch ID or your password, then shows a code to type in that terminal.")
                } else {
                    Button(unlock.isActive ? "Lock" : "Cancel") { Task { await model.lock(unlock) } }
                }
            }
            .controlSize(.small)
            .disabled(model.busy.contains(unlock.id))
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 6).fill(Color.primary.opacity(0.05)))
    }

    private var symbol: String {
        switch unlock.state {
        case .pending: "lock.trianglebadge.exclamationmark"
        case .confirmed: "keyboard"
        case .active: "lock.open"
        }
    }

    private var title: String {
        switch unlock.state {
        case .pending: "A terminal asks for operator scope"
        case .confirmed: "Confirmed: waiting for the code"
        case .active: "Operator unlock in force"
        }
    }
}

/// Confirms with Touch ID or the login password (LocalAuthentication's
/// device-owner policy). A program that clicks the app's buttons cannot
/// pass it.
struct DeviceOwnerConfirmer: UnlockConfirmer {
    func confirm(reason: String) async -> Bool {
        let context = LAContext()
        var error: NSError?
        guard context.canEvaluatePolicy(.deviceOwnerAuthentication, error: &error) else { return false }
        return (try? await context.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason)) ?? false
    }
}

/// Tells the user a terminal asks for an unlock. One notification at a
/// time (a later request replaces it), so a stream of requests cannot flood
/// the screen; it names the process and duration only, and the command is
/// shown in the panel.
@MainActor
func postUnlockRequests(_ requests: [OperatorUnlock]) {
    // UNUserNotificationCenter traps outside an app bundle (see Notifier).
    guard Bundle.main.bundleIdentifier != nil, let latest = requests.last else { return }
    let content = UNMutableNotificationContent()
    content.title = requests.count == 1 ? "A terminal asks for operator scope" : "\(requests.count) terminals ask for operator scope"
    content.body = "Process \(latest.pid) asks for \(latest.durationText). Confirm or deny it in Orch's menu bar panel."
    content.threadIdentifier = "unlocks"
    UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: "unlock-pending", content: content, trigger: nil))
}
