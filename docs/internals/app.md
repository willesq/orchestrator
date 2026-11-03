# macOS app

`Orch.app` is a menu bar app for orchd. It is a client like `orch`: it dials the daemon's Unix
socket over gRPC and never opens `orch.db`, `workflows.db` or Qdrant itself, so every rule the
daemon enforces (redaction, delete confirmation, migration locks) applies to it too. The source
lives in [`macos/`](../../macos/), a Swift package.

## Layout

| Target | Contents |
|---|---|
| `OrchKit` | Everything testable: the generated orchd client (`Generated/`), the Dashboard timeline (`Activity`), the connection (`DaemonClient`), health classification (`Health`), polling (`DaemonMonitor`, `PRMonitor`), PRs, attention and the tray icon (`PullRequest`), memory search and sources (`Memory`, `MemoryModels`), tasks and dispatch (`Tasks`, `TaskModels`), GitHub scopes and activity (`GitHub`, `GitHubModel`), the PRs board lanes (`PRBoard`), handing PRs to agents (`PRHandoff`, `PRHandoffModel`), system metrics, logs and the status report (`System`, `SystemModel`), docs and registered sources (`Catalog`, `CatalogModels`), the safety net (`SafetyNet`), command palette matching (`CommandPalette`), companion window geometry (`CompanionLayout`), fitting panes to the window (`AdaptiveLayout`), markdown rendering (`MarkdownRenderer`), the login item (`LoginItem`), daemon control (`DaemonControl`), and socket path resolution (`OrchPaths`) |
| `OrchApp` | The SwiftUI `MenuBarExtra`, the main window (`MainWindow`, `DashboardView`, `ActivityTimeline`, `SystemView`, `LogsView`, `MemoryView`, `DocsView`, `SourcesView`, `TasksView`, `GitHubView`, `PRsView`, `PRBoardView`, `PRHandoffCard`, `SafetyNetView`), the ⌘K command palette (`CommandPaletteView`, `PalettePanel`), the companion column (`CompanionView`, `WindowControl`), panes that fit any width (`AdaptiveSplit`, `FlowLayout`), and notifications (`Notifier`) |
| `OrchKitTests` | Swift Testing suites, including a round trip against a fake orchd on a temporary socket |

## Connection

- **Socket path:** the same as `internal/paths`: `ORCH_SOCKET`, else `ORCH_DATA_DIR/orchd.sock`,
  else `$XDG_DATA_HOME/orch/orchd.sock`, else `~/.local/share/orch/orchd.sock`.
- **Connection:** the app keeps one HTTP/2 connection (grpc-swift 2, POSIX transport, plaintext).
  The socket's 0600 mode is the trust boundary, as for every other client. The transport
  reconnects when orchd restarts and recreates the socket.
- **Deadlines:** each call has a 3 s deadline, matching `orch status`.
- **Codegen:** `make proto` generates both the Go and the Swift client code. The Swift protoc
  plugins are built from the versions pinned in `macos/Package.resolved`, so generated code
  always matches the runtime the app links. CI fails if the committed Swift no longer matches
  `orchd.proto`.

## Health

`DaemonMonitor` calls `Status` every 10 s. The tray icon shows one of four states:

| State | When |
|---|---|
| unknown | before the first check |
| healthy | reachable, and no unit is `failed` or carries an error |
| degraded | reachable, with at least one `failed` unit or one recording an error; `stopped` does not count, because stopping a unit is deliberate |
| down | the call failed (no socket, refused, or past the deadline) |

## Needs you

`PRMonitor` calls `ListPRs` every 60 s. The babysitter refreshes its cache every 10 minutes, so
polling the cache costs nothing. The menu shows up to eight PRs that need the user, most urgent
first, each with its reason (see the PR babysitter in [tasks.md](tasks.md)). Clicking one opens it.

- **Opening PRs:** the app only opens URLs of exactly the form
  `https://github.com/<owner>/<repo>/pull/<n>`: no port, credentials, query or fragment. It hands
  them to the system to open, so nothing else should get through.
- **Badge:** the icon becomes a bell with a count while orchd is healthy and something needs the
  user. A degraded daemon keeps its warning icon with the count. A down or unknown daemon hides
  the count, because the list would be stale.
- **Notifications:** one per PR that newly needs the user, or now needs them for a different
  reason. Nothing is posted for what is already waiting at launch. More than three at once become a
  single "N PRs need you" summary. Clicking a PR's notification shows that PR in the GitHub tab's
  PRs pane; the summary shows the PRs pane narrowed to Needs action. A new review verdict posts its
  own notification ("Review verdict: Approve", the repo and number, the reason cut to 240
  characters), polled by `ReviewMonitor` from `ListPRReviews` every 20 s for the whole app; none is
  posted for verdicts that already existed at launch. A failed poll keeps the
  last good list, so a briefly unreadable cache cannot make every PR look new. Notifications need the app bundle, so a bare `swift run` binary skips them.

## Launching

Orch.app lives in the menu bar (`LSUIElement`). Without help, opening it would show nothing, and on
a crowded menu bar its icon can sit behind the notch. `AppDelegate` handles this:

- **When the user opens it** (Finder, Spotlight, `open`), the main window appears. A launch at login
  (the launch Apple event's `keyAELaunchedAsLogInItem`) shows only the tray icon.
- **Opening it again** while it runs brings the window back.
- **One copy only:** the earliest-launched live copy stays, with the lowest PID breaking a tie
  (`LaunchPolicy.shouldHandOff`), so copies started together leave exactly one. Any later copy posts
  the `com.willesq.orch.show-main-window` distributed notification, which makes the running copy
  show its window, and quits without opening a connection to orchd.
- **The notification:** it carries nothing, so whoever posts it can only bring the window forward.
  The running copy honours at most one request a second.
- **Plain AppKit window:** the main window is an `NSWindow` hosting the SwiftUI `MainWindow`. It
  remembers its frame, and the selected pane is kept in user defaults. The hosting controller's
  `sizingOptions` is empty, so the window keeps the size the user gives it instead of following each
  pane's ideal size. Every pane has toolbar content, so the toolbar band never comes and goes, which
  would otherwise change the window's height on a pane switch. A SwiftUI `Window` scene can
  only be opened from a live view, and the menu bar label is rendered as an image, so it cannot do
  that for AppKit.
- **Dev builds:** `make app` writes to `build/app.noindex/`. The `.noindex` suffix keeps Spotlight,
  and so "open Orch", from launching a dev build in place of the installed app.

## Main window

"Open Orchestrator…" in the menu, or opening the app, shows a window with a sidebar.

- **Look:** one shared set of pieces in `Theme.swift`: a small type scale (monospace only for
  commands, paths, code and rule tokens), secondary text raised to WCAG AA contrast in both
  appearances, one raised card surface, chips, status dots, and an empty state with its primary
  action. Everything uses system colors, so light and dark mode follow the system.
- **Appearance:** follows the system's light or dark mode by default. A picker at the foot of the
  sidebar and in the menu bar panel keeps Light or Dark instead (`appearance` in user defaults,
  applied to the whole app, menu bar panel included, at launch and on change).
- **Sidebar badges:** Docs shows how many repos failed to sync, GitHub how many PRs need you,
  Safety Net a dot in its state's color, and Claude an orange dot when items were removed or are
  missing from their files. They come from the models the tabs use; a `PeriodicRefresh` reloads
  docs, Safety Net and settings every five minutes, and the PR count follows `PRMonitor`.
- **Command palette (⌘K):** one keyboard surface over the whole app, from any pane. It lists, most
  pressing first, the PRs that need you, actions (New task…, which opens the dispatch sheet on the
  Tasks pane; Dock as companion or Expand; Keep Orch on top; Refresh everything; Show PRs that need action; each appearance), every pane with its
  sidebar status, the other cached PRs, and the 30 most recent tasks. Typing filters by fuzzy match
  (`PaletteMatcher`: characters in order, word starts and unbroken runs score higher), and results
  stay grouped under their section. ↑↓ move, ↩ runs, Esc or ⌘K closes, and clicking outside
  dismisses it. It runs in its own floating panel (`PalettePanel`) so the pane behind cannot keep
  the keyboard. It only navigates and opens what the panes already offer: a PR opens in the PRs
  pane, never the browser, and nothing it runs dispatches, cancels or deletes without the pane's
  own confirmation.
- **Companion mode (⌘⇧D):** the window is built to sit beside the terminal the agents run in.
  Narrower than 600pt (`CompanionLayout.compactWidth`), whether the user dragged it there or a
  window manager tiled it (they resize through Accessibility, which ignores the minimum size, so
  the sidebar layout used to clip), it shows one column instead of the sidebar and a pane:
  orchd's health, the PRs that need you (a click opens the PR in the PRs pane), the agents at work
  with Cancel (confirmed first) plus the three most recent finished tasks, and a strip of every
  pane with its sidebar badge. A click on a PR, task or pane widens the window and shows it. The
  window goes down to 320×420. ⌘⇧D, the toolbar's Companion button, the column's dock button or
  the palette docks it: a full-height 380pt column at its screen's right edge, kept above other
  windows and on every Space. ⌘⇧D again (or Expand) restores the frame it had before docking; a
  window the user narrowed by hand widens to at least 980×640 toward the screen's centre, kept on
  screen. The pin keeps an undocked window above others too. A pinned window shows on every Space,
  so its PR titles and task headlines appear in screen shares there too. Docking and pinning
  last until the app quits.

- **Fitting the window:** no pane reaches past the window's edge. Every list-and-detail pane (Docs,
  Sources, Tasks, Claude, the PRs list and board) is an `AdaptiveSplit`: side by side while both
  columns' minimum widths fit, otherwise one at a time, the detail replacing the list under a Back
  bar (⌘[) while something is selected. GitHub's Overview switches between Scopes and Activity with
  a segmented control instead. Rows of filters and chips (Memory's search filters, the PR filters,
  the Dashboard's summary chips) wrap with `FlowLayout`; System's tiles flow in an adaptive grid;
  Claude's section picker turns into a menu when its segments do not fit. If something still cannot
  fit, the window cuts it at the trailing edge, so the sidebar always stays whole. The window's
  content starts at the window's size, so a launch does not lay out the companion column first and
  rebuild every pane; a load cancelled because its pane went away is not reported as an error.

- **Dashboard:** orchd's health, then summary chips for Docs, Safety Net and PRs that open their
  tabs, the Needs you list, the Activity timeline, and a chip per subsystem.
  - **Activity:** what orch did on its own, newest first and grouped by day (`ActivityFeed`): each
    task at its last update (status, agent, duration, cost, PR count), each watched PR's review
    verdict or current review state, the last docs sync plus one entry per failing repo, and each
    registered source's last refresh. orchd keeps no event log, so an entry is a record's latest
    state, not its history: a task retried twice shows once. Filters (All, Tasks, Reviews, Docs,
    Sources) are kept in user defaults; twelve entries show until **Show more**. Clicking an entry
    opens its task, PR, Docs tab or source. While the timeline shows it reloads tasks (with the
    Tasks tab's status filter) and sources every 15 s; reviews and docs come from the app-wide
    polls. **Live** shows while any entry is still running.
  - **Subsystems:** one chip per unit, red when failed with the error on hover, and **Restart** on
    right-click; **Details** opens the System tab and **Restart all** stays in the card header (the
    `Restart` RPC, one at a time).
- **System:** an Overview/Logs switch in the toolbar. Both poll only while the tab is shown.
  - **Overview:**
    - **Process tiles:** CPU and memory for orchd and qdrant (`GetSystemMetrics` every 2 s), each
      with a sparkline scaled to its own range, so a steady value is a line rather than a block.
      While the tab is hidden the app samples metrics every 10 s, so the lines open with history.
      A process that stops or comes back under a new PID starts a new line.
    - **Runtime:** orchd's goroutines and Go heap.
    - **Subsystems:** each unit's heartbeat age, ticking every second, with a menu (the ellipsis, or a
      right-click) with **Restart**.
    - **Storage and tasks:** on-disk store sizes and task counts by status.
    - **Status report:** the full `orch status --json` report every 15 s. The header counts the
      lines that warn or fail ("2 need attention"), so a warning is never summed up as no problems. The app runs `orch`
      from an argument array, as for daemon control, so the report covers the checks that work
      with orchd down.
  - **Logs:** `TailLogs` every second.
    - **Buffer:** keeps the latest 5000 lines from every source.
    - **Controls:** a source picker shows one source or all of them. A text filter, **Pause**
      (resuming catches up) and **Clear** sit beside it.
    - **Gaps:** if lines rolled out of orchd's memory before the app read them, a note says how
      many.
    - **Restarts:** when orchd restarts, its numbering starts over, so the view starts over too.
    - **Text:** log text is shown verbatim and never rendered.
- **Docs:** what `orch docs` keeps in memory: the synced owners' docs as the store holds them, by
  repo, with a filter and each doc's chunk count (`ListDocs`).
  - **List:** each repo is a heading with its files under it and each file's chunk count.
  - **Sync status:** while orchd syncs, the progress (repos done of total). When the last sync
    failed or stopped early, a banner says so in a headline, one line with the most common cause,
    and **Sync now** (`SyncDocs`). The disclosure shows the owner's reason and the failing repos
    grouped by cause, in a capped scrolling area, which shows orchd's answer when a sync is already
    running. A failing repo's header carries a ⚠ with its reason. Paths and addresses show control
  and bidi characters as escapes, and an address containing them cannot be copied.
  - **Reading:** choosing a doc rebuilds it from its chunks (`GetDocument`). A callout at the top
    says it is a rebuild, close to the original but not exact. It is shown in a read-only text
    view that lays out only what is visible, so a large doc does not block the window.
  - **Rendered or Source:** markdown (synced and repo docs, and any `.md` source) is rendered by
    default with `swift-markdown`, off the main thread: headings, emphasis, lists and task lists,
    quotes, code, tables as aligned rows. Source shows the stored text as is. The rendering is
    inert, because the content is untrusted:
    - images are never loaded; their alt text is shown;
    - raw HTML is shown as text, never interpreted;
    - only plain `http`/`https` links are clickable, relative ones resolved against the doc's
      GitHub location at the indexed commit, and a click is checked again before it opens.
    - a link whose text names another host shows the real host beside it.

    Rendering runs on its own thread with a 64 MB stack, because the parser recurses once per
    level of nesting: markdown nested deeper than 64 quote levels, or with a run of more than 256
    emphasis or link markers, is shown as its source instead, and nesting past 32 levels renders
    as plain text. Table cells are cut to 80 characters, and a table that would pad past 256 KB is
    shown unpadded. Choosing another doc cancels a render in progress.
  - **Open on GitHub:** opens the file at the commit that was indexed, and only an exact
    `https://github.com/<owner>/<repo>/blob/<ref>/<path>` URL.
  - **Search:** within this doc (`source_uri`) or this repo (`all_tags` `owner:` and `repo:`). A hit
    in another doc of the repo opens that doc.
- **Sources:** the sources registered for refresh on a cadence: web pages, local repos and local
  owner folders. With none registered, the tab shows an empty state with **Add source** instead of
  an empty table.
  - **List:** each source's status, refresh interval and last fetch.
  - **Add and edit:** a sheet that checks orchd's rules before sending (`SaveSource`); an edit
    that changes the address replaces the entry.
  - **Refresh now:** for one source or all (`RefreshSource`).
  - **Remove:** a confirmation that offers to keep or delete what the source indexed
    (`RemoveSource`, with `purge` and `confirm` only after that choice).
  - **Open:** a web source's page, only for plain `http`/`https` URLs.
  - **Search:** within the selected source, using the filter orchd returns for it.
- **Memory:** a Search/Indexed switch in the toolbar.
- **Memory → Search:** the `Search` RPC with labelled menus: **Match** (as configured, words and
  meaning, meaning only), **Type** (the source types seen in the store) and **Show** (5 to 100
  results). Each hit shows a short source name, its type chip, date and score, the full address,
  and a preview with blank lines collapsed; **Show more** appears only when the text is long.
  Before the first search, the empty state lists this launch's recent searches.
  - Indexed text is untrusted, so it is shown verbatim and never rendered as Markdown.
  - A newer search cancels an older one, and a cancelled search that finishes late cannot replace
    newer results.
  - Searches get a 15 s deadline instead of 3 s, because the first search after a restart loads
    the embedding model.
- **Memory → Indexed:** `ListSources`, every source in memory, up to 200 at a time with totals,
  filtered by type.
- **Tasks:** follows `WatchTasks` while the tab is shown, so rows change the moment a task's status
  or liveness changes; no polling.
  - **Stream:** up to 100 tasks, filtered by status. Changing the filter restarts it. Each stream
    opens with a snapshot that replaces the list, then sends changed tasks. A tick every 15 s
    refreshes liveness.
  - **Reconnects:** when orchd restarts the stream ends, and the app reopens it with backoff
    (1 s, doubling to 30 s).
  - **Older orchd:** one without `WatchTasks` (`unimplemented`) is polled with `ListTasks` every
    10 s instead.
  - **Empty:** with no tasks, the tab shows **Dispatch task** (or **Show all tasks** under a
    filter) and no detail pane.
  - **Stalled:** rows mark ⚠ stalled tasks.
  - **Detail:** the selected task's model, run models, duration, turns, cost, verification and PR
    outcome, plus its body and result, shown verbatim.
    - **Liveness:** for a pending or in-progress task, running (with its PID), waiting or stalled,
      when the session started, last activity, session ID and working directory.
    - **Actions:**
      - **Retry** (`RetryTask`) for an ended or stalled task.
      - **Cancel…** (`CancelTask`), with a confirmation.
      - **Copy Resume Command** copies orchd's shell-quoted `claude --resume` line to the
        clipboard. Nothing is run on your behalf.
  - **PR links:** opened only through the same `github.com/<owner>/<repo>/pull/<n>` check.
  - **Rating:** a finished task can be rated good, needed rework or bad (`RateTask`), which feeds
    model routing.
- **GitHub:** what orch monitors on GitHub, and what it found.
  - **Watching:** your account ("You") and each org you belong to ("Org"), each with an on/off
    switch. An enabled scope also shows:
    - a picker between "Involving me" and "Everything open";
    - with "Involving me", **Everything in…**, a sheet to choose repos watched for every open PR and
      issue on top of that;
    - a summary of what it covers, and **Repos…**, a sheet to choose all repos, only some, or all
      but some, from the repos you can see.

    Changes go to `SetGitHubScope`. orchd then sweeps within about a minute, and the activity list
    reloads every 30 s while the tab is open.
  - **Your teams:** from `GetGitHubIdentity`.
  - **Activity:** `ListGitHubItems`, filtered by All, PRs or Issues in the toolbar, in two sections:
    **Needs action** and **Everything else**, each newest first. Each row shows its title,
    `repo#number`, author, age, and its reasons as chips; a reason its attention already states is
    not repeated. Items untouched for six months that need nothing are shown quieter. An empty
    filter says what it hides and offers **Show everything**. Clicking opens the item, but only exact
    `https://github.com/<owner>/<repo>/pull|issues/<n>` URLs. Titles and authors are other people's
    text, shown verbatim.
  - **Refresh** calls `RequestGitHubSweep` and shows a spinner until `ListGitHubItems` reports a
    newer `updated_at`, so a new PR or review request usually appears within about a minute instead of at
    the next 10-minute sweep. If nothing lands in two minutes (gh failing, rate limited, the watcher
    off) a banner says the list may be out of date; orchd does not report why. On Overview it also
    asks GitHub for your orgs and teams again. **Refresh everything** in the command palette makes
    the same request.
  - **Sections:** an Overview/PRs switch in the toolbar, kept across launches. Overview is the
    above. PRs shows "PRs (n)" while n verdicts wait on you.
  - **PRs:** the PRs from `ListGitHubItems`, needing-you first, then newest.
    - **Filters:** a title or `#number` search; role (Any, Mine, Review requested, Involved); state
      (All open, Needs action, Drafts, CI failing, Watched for review, Has a verdict); repo. Each
      row carries its attention, Draft, and its review (Watching, Reviewing, or the verdict).
    - **Layout:** a List/Board switch in the toolbar, kept across launches. Board lays the
      filtered PRs out in lanes by where each stands (`PRBoard`): **Fix** (CI failing, changes
      requested, conflicts, or a Changes needed verdict), **In flight** (drafts and running CI),
      **Review** (waiting on a reviewer or orch's review), **Ship** (ready to merge, or an Approve
      verdict), and **Cold** (untouched for 180 days and not waiting on your review), folded to a
      strip until clicked. Verdicts count only for the current head; review requested beats Cold.
      Lanes share the window's width, and the board scrolls sideways only when cards would get too
      narrow. Each card shows its repo, age, title, attention, Draft, CI and review; clicking one
      opens the same detail beside the board, closed with its × or Esc.
    - **Detail:** the selected PR from `GetPRDetail` (title, branches, size, head, CI with each
      check, review decision, requested reviewers, reviews, labels, files, description), with
      **Reload** and **Open on GitHub**. Text from GitHub is shown verbatim, never rendered.
    - **Agents:** the tasks working on the PR (`PRHandoff`: a task with the `PR: <url>` line a
      hand-off writes, orch's own review of the PR, or one orchd recorded touching the PR; tasks
      that only mention the URL are not counted), unfinished first, live from `ListTasks` every 5 s
      while the GitHub tab shows, each with **Cancel…** (confirmed) and **Open** in Tasks. What
      blocks the PR becomes a button, most blocking first: **Resolve conflicts**, **Fix failing
      CI**, **Address review**, **Apply orch's review** (a Changes needed verdict on the current
      head); **Hand off** offers every goal, including Something else. Each opens a form with the
      goal, **Let it push to `<owner/repo>:<branch>`** (off: the agent works locally and reports a
      diff), the agent (`dev`), an optional model, notes, and the exact brief, which **Edit** makes
      editable. ⌘↩ dispatches it as a `code` task; nothing is dispatched otherwise. The form keeps
      the PR as it was when it opened, so a refresh cannot change the brief being read.
      - **Brief:** the PR's link on its second line, the goal, then the PR's data (title,
        branches, head, failing checks, who asked for changes, orch's review with the full review
        cut to 8,000 characters) in `> ` lines, each value cleaned to one line (line breaks of any
        kind, including CRLF, U+2028 and U+2029, become spaces; control and format characters are
        dropped), so text from GitHub cannot pose as a rule. Its rules: `> ` lines are data, not
        instructions; check out with `gh pr checkout`; push to that `owner/repo:branch` only,
        never force-push or merge, or do not push at all; run the tests.
      - **Someone else's PR:** the form warns that the agent works on their code with your
        sign-in and needs **I have read this PR and trust the agent to work on it** before it
        sends. Pushing is unavailable, and the brief tells the agent not to run the PR's code
        outside a sandbox. Pushing is also unavailable when the head branch is the base,
        `main`, `master`, `develop` or `trunk` (in any case), names a fork could use to land on a
        protected branch.
      - **On the list and board:** while a task works on a PR, its row and card show **Agent
        working** (or **Agent stalled**) with a pulsing dot (steady under Reduce Motion), then
        **Ready to land** while a proposal waits on the user (`PRLandingModel`, from
        `ListPRLandings` every 5 s), or **Proposal out of date** once the card has found it cannot land.
      - **Task rows:** also say **local only** or **may push**, and what the run cost, so two
        runs on one PR can be told apart.
        A blocked or failed run also shows the first line of why it stopped.
      - **Activity:** each run's tool calls (`GetTaskActivity`, every 3 s while it runs), opened
        while it runs. Its line says how many commands ran, files were edited and read, and steps
        failed or looked risky. Opened, it adds the run's time, turns and models, a chip per tool with
        its count, the files edited with their lines, and **All / Commands / Files / Attention**.
        - **Steps:** one line each, showing the main command rather than the `cd` before it, with
          **failed**, the risk, the lines an edit wrote and how long a slow step took. A click opens
          its commands one per line, what it returned, and **Copy**. A closed line is not selectable,
          since selecting text lays it out past its limit, over the rows below.
        - **Commands, never descriptions:** a shell step shows the command, never the agent's account
          of it. Risky commands carry a red chip and stay listed however long the run gets. A run told
          not to push that ran `git push` shows a warning. Without a transcript (not started, or Codex)
          it says so.
      - **Landing:** a finished run whose result ends with a proposal (see
        [tasks.md](tasks.md#pr-landings)) shows a panel, reloaded when the PR's head changes:
        - **Ready to land:** the commits, the change's size, **Review diff** (the real diff,
          coloured line by line, shown verbatim), **Push to `<branch>`…** (disabled until that diff
          has been opened; confirmed, naming the commit and size), **Send back…** and **Discard**.
          Why it cannot land, a diff cut off at its limit and any binary files all show here.
        - **Send back:** the user's feedback and the exact brief for a new local-only run;
          queuing it discards this proposal. ⌘↩ sends.
        - **The PR moved on / cannot land:** orchd's reason, with Send back and Discard.
        - **Already on the PR:** the commit reached GitHub some other way than Push here.
        - **Pushed:** **Undo…** (confirmed) while the commit is still the branch's head; otherwise why not.
        - **Undone**, or **Discarded** with **Restore**. The agent's worktree is never removed.
        - **Failures:** what orchd or GitHub said, with **Check again**.
      - **Something else:** needs notes or an edited brief before it sends, since an agent sent
        off with no instructions can only stop and ask.
    - **Description:** rendered as Markdown with the Docs renderer: inert (no images loaded, raw
      HTML shown as text, links opened only after the same checks), and shown as its source when
      nested too deeply to render safely.
    - **Review:** two permissions, which for a watched PR always show the mode orchd holds, "approve the PR, with its comment if it has one" (unavailable on
      your own PR) and "comment its suggestions when it does not approve"; both off means orch
      asks you each time. **Notes for the reviewer** are added to the review prompt and never
      posted. **Review when
      ready** starts the watch (`WatchPRReview`).
    - **Held:** a PR by someone who is not the repo's owner, a member or a collaborator waits for
      **Review anyway…** (after a confirmation that it runs their code with your sign-in), and a
      notification says so. The list marks it Held.
    - **Progress:** while watched, a row of stages (Waiting until ready, Reviewing, Verdict,
      Posted), orchd's note on what it is waiting for, and "Checked … ago · next check in …",
      ticking every second, with **Check now**. While reviewing, the card follows the review task
      every 10 s (`GetTask`): running, waiting or stalled, how long it has run, and its last
      activity.
    - **Verdict:** the verdict for its commit, the reason, a **For you** box with your notes and
      the review's answer to them, the **Full review** (rendered, collapsed), and the draft comment with
      **Preview** (rendered Markdown) and **Edit**. **Post comment** has orchd post the draft
      once. Widening the permissions or posting asks first, and posts the draft as it was when you
      confirmed; narrowing them never asks. Saving changed notes reviews the commit again. The
      menu stops watching, or stops and forgets the verdict (`StopPRReview`). What orch posted, or
      why it could not, is shown with the verdict.
      stops watching, or stops and forgets the verdict (`StopPRReview`). What orch has posted, or
      why it could not, is shown with the verdict.
- **Claude:** the managed settings ([settings.md](settings.md)): Claude Code's hooks, permission
  rules and plugins, cc-safety-net's rules, and what orch ships for agents, in seven sections
  (Hooks grouped by event, Permissions by list, Plugins by marketplace, Safety Net rules by
  rulebook, Instructions by topic, Subagents, Skills), each with how many are on. Instructions,
  Subagents and Skills show each item's full text, rendered, in the detail pane.
  - **Live:** `ListSettings` every 2 s while the tab is shown, so edits made to the files elsewhere
    appear within a few seconds; nothing is redrawn when the listing has not changed.
  - **Rows:** a switch, a readable name, the exact text under it, and an origin badge (`orch`,
    `you`, `external` for what was already in the file). A hook is named after what it runs: known
    tools (Orca's `.orca/agent-hooks` wrapper, cc-status, cc-safety-net, rtk) by name,
    `orch-hook` and `serena-hooks` with their subcommand, anything else by its program's base name.
    Its matcher is a chip, and the full command is a truncated monospace line (in full in the
    detail pane). The section picker reads "Hooks · 37 on" or "Hooks · 35 of 37 on".
  - **Filter:** the toolbar's search field keeps items whose name, command or matcher match. A removed or missing item says why. A
    change in flight shows a spinner; a refusal (for example, the file changed underneath, or
    the file does not parse) is shown under that row.
  - **Detail:** the selected item's group, ID, origin, when it changed, and the exact JSON written
    to the file, with **Remove…** (confirmed) for items you added or orch found. An instruction
    module instead shows its title, description, topic and installed path, and its full text
    rendered as Markdown (`ModuleDetail`, with `MarkdownRenderer`), so it can be read without
    opening the file.
  - **Add:** the toolbar's Add opens a form built from the kind's fields as orchd describes them
    (hooks and permission rules); required fields and whole numbers are checked before sending,
    and orchd's refusal is shown in the form.
  - **Scope:** the header shows the file and "This Mac only": choices live in this machine's
    `orch.db`.
  - **Reuse:** `SettingsModel` (OrchKit) takes the provider names it shows, and the list, row,
    detail and add-form views in `SettingsComponents.swift` take a provider and kind, so a tab over
    another tool's settings reuses them. Every string from a file goes through `displaySafe`.
- **Safety Net:** cc-safety-net in Claude Code (`GetSafetyNet`, read when the tab opens and on
  **Reload**).
  - **State:** a badge (Protected, Rules incomplete, Not installed, Old copy installed, Update
    available, Disabled), the installed and pinned version with the pinned commit and release
    date, any competing copy, what is wrong with the rules, and plugin files that could not be
    read.
  - **Rulebooks:** every source `rule.json` lists, in order, marked orch or yours. Each rule reads
    as one phrase ("Blocks `git worktree add`") with its reason under it, and has the same switch
    as the Claude tab: it goes through the cc-safety-net settings provider (`SetSettingEnabled`
    on `<rulebook>/<rule>`), so both tabs agree. A rulebook cc-safety-net would skip shows why.
  - **Actions:** only the action that applies is shown, as the primary button: **Install**
    (missing, old copy, or disabled), else **Upgrade** (outdated), else **Repair rules** (rules
    not ok, rule files only); otherwise the header says "Up to date". Each sends `InstallSafetyNet`
    with a 10-minute deadline.
    While it runs, the tab polls `GetSafetyNet` every second for the current step; afterwards it
    shows the result, the error, and the command output. orchd's refusal when an action is
    already running is shown as is.
  - **Text:** rule names, reasons, paths, errors and command output come from files and commands,
    so all of it goes through `displaySafe`.
- **Dispatch:** a sheet that sends `Dispatch` with an agent, an optional task kind (showing what
  `RecommendModel` would route it to), an optional pinned model, an optional working directory
  (chosen with a folder picker) and the task body.
  - **Validation:** it checks orchd's own rules before sending: the agent name pattern, a non-empty
    body up to 64 KiB, and an absolute `cwd`. orchd still validates on its side.
  - **Sending:** ⌘↩, not plain Return, so pressing Return in a text field never starts an agent.
  - **After sending:** the form clears the body, so a second click cannot queue the task twice.
    It shows the route orchd actually chose, and warns when no worker is enabled, because the
    task then waits.
  - **Lost answers:** dispatch gets a 30 s deadline, because routing may build a live proposal
    first. If the answer is lost (deadline, dropped connection), the task may already be queued,
    so the form says so, keeps the body, and refreshes the task list instead of inviting a blind
    retry.
  - **Late results:** a send that finishes after the form closes is dropped, so it can't bring
    back a stale result.
  - **Task kinds:** the form's list copies `routing.Kinds`, and a Go test fails if they drift.

`SearchModel`, `SourcesModel`, `RestartModel`, `TasksModel` and `DispatchModel` in OrchKit hold this
logic, behind small `MemoryAPI`, `ControlAPI` and `TasksAPI` protocols so views and tests depend only
on what they use.

## Open at login

`LoginItem` wraps `SMAppService.mainApp`, and the menu's "Open at login" checkbox turns it on or
off. If macOS registers the app but wants the user's approval, the menu links to System Settings →
Login Items. The state is re-read every time the menu opens, because it can also change in System
Settings. A binary running outside an app bundle cannot register, so the checkbox is disabled there.

## Daemon control

**Start** and **Restart** run `orch daemon start|restart`, which already chooses between brew
services and launchd. An app launched from Finder gets launchd's minimal `PATH`, so it looks for
`orch` in `/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin` and `~/go/bin`. The command runs
from an argument array, never through a shell. `orch` itself adds the same Homebrew and `~/.local/bin` directories to its
`PATH` at startup, as orchd does, so the `gh` and `brew` it runs are found even with launchd's
minimal `PATH`.

## Brand

The logo is two SVG files in `macos/Resources/Brand/`:

- **`icon.svg`:** the app icon. Three rings, one per coding agent (Claude Code, Codex, Gemini),
  overlap on the memory they share. The art fills Apple's 824 pt icon grid inside a 1024 canvas.
- **`glyph.svg`:** the 18 pt menu bar glyph. The rings sit further apart than in the icon so they
  stay distinct at that size.

`build-app.sh` renders `icon.svg` with AppKit's own SVG support (`scripts/render-icon.swift`) into
every size `iconutil` needs. It packs them into `Contents/Resources/AppIcon.icns` (named by
`CFBundleIconFile`) and copies the glyph in as `MenuBarGlyph.svg`. No extra tools are needed.

Where the logo appears:

- **Menu bar:** the glyph, as a template image macOS tints, while orchd is healthy, with the
  needs-you count beside it.
- **Tray menu and Dashboard headers:** the app icon while orchd is healthy (`HealthMark`).
- **About Orch:** the menu opens the standard About panel, which takes the icon and version from
  the bundle. A menu-bar-only app has no app menu to reach it from.
- **Finder, Login Items and notifications:** macOS shows the bundle's icon.

Degraded, down and unknown replace the logo with their warning symbol in its color, because those
states must stand out. The README uses `icon.svg` directly. To change the logo, edit the SVGs and
rebuild.

## Build, sign, release

- **`make app`:** assembles `build/app.noindex/Orch.app`, ad-hoc signed by default. Set
  `SIGN_IDENTITY` to a Developer ID Application identity to sign with the hardened runtime and a
  secure timestamp, which notarization requires.
- **`make app-install`:** builds the app and installs it as `/Applications/Orch.app`, where Finder,
  Launchpad and the cask put apps. It uses `~/Applications` when `/Applications` is not writable,
  and `INSTALL_DIR` overrides both.
  - It first asks a running copy to quit, then replaces the bundle and launches the new one;
    `NO_LAUNCH=1` skips the launch.
  - It removes a copy an earlier install left in the other folder, but only one whose bundle id is
    `com.willesq.orch`, so Spotlight and Login Items see a single Orch.
  - This is the way to run the app from source until the cask ships.
- **Bundle settings:** `LSUIElement` keeps the app out of the Dock, and `LSMinimumSystemVersion`
  is 15.2.
- **Tests:** `make app-test` runs the tests, and `make app-cover` applies the 80%-per-file gate
  to `OrchKit` (generated code excluded).
- **CI:** `app.yml` runs on pull requests to `main` that touch `macos/**`, `orchd.proto` or the
  Makefile. It checks codegen drift and runs the coverage gate; it does not assemble or upload
  the app, since the release builds its own. No checks run on `main`: the PR is the gate. To try
  a branch's app, build it locally with `make app`.
  - **Build cache:** the Swift package build (`macos/.build`, dependency checkouts, the protoc
    plugins) is cached as `swift-<main|branch>-macos-26-<Xcode>-<SHA of Package.resolved>`. A PR
    restores its own cache, else the one `cache-main.yml` saved on `main` when `Package.resolved`
    last changed. The release workflow restores no cache.
- **Release:** the `app` job in `release.yml` runs after the binaries are released and always
  attaches `Orch_<version>_darwin_arm64.zip` to the release. The build happens before any signing
  material is on the runner, and only versions pinned in `Package.resolved` are used.
  - **With the Apple secrets:** it imports the Developer ID certificate into a throwaway keychain,
    re-signs, notarizes with an App Store Connect API key and staples. It then dispatches the tap's
    `bump-cask.yml` with the version and sha256 (the cask URL carries the version).
  - **Without them:** the zip is ad-hoc signed, and the cask is bumped only when the repository
    variable `ORCH_APP_CASK_ADHOC` is `true`, because Gatekeeper blocks an ad-hoc app that Homebrew
    downloaded until the user allows it.
- **Installing:** the `orch-app` cask in `willesq/tap` depends on the `orch` formula, which
  stays the owner of the binaries and of `brew services`.
- **Runners:** the Swift dependencies (swift-crypto, swift-log, swift-service-lifecycle) need
  Swift 6.2, so the app jobs run on `macos-26` and select its newest Xcode 26.
