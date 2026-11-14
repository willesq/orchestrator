package spaces

// The outbound terms check's baseline (SP-044, D-60): the behaviours that
// decide how strictly a command is read, each with a stable id. The ids
// are the contract SP-075 builds on to let the operator switch them; until
// then every rule here is always on, and the code that applies one names
// its id beside it.

// OutboundRule is a stable baseline rule id. Never rename or reuse one.
type OutboundRule string

const (
	// OutboundFailClosed: while the terms layer is on, an outbound command
	// the check cannot read in full is refused as outbound_unchecked.
	OutboundFailClosed OutboundRule = "fail_closed_unreadable"
	// OutboundGHDefaultRepo: a gh write that names no repo goes to the repo
	// gh itself would use (GH_REPO, else the checkout's default repo set
	// with gh repo set-default, else its base remote as gh picks it), not
	// to every remote of the checkout (D-60).
	OutboundGHDefaultRepo OutboundRule = "gh_default_repo"
	// OutboundPlainVariables: $PWD, $USER and $HOME (also as ${NAME}) are
	// read as the shell would expand them instead of failing closed (D-60).
	OutboundPlainVariables OutboundRule = "plain_variables"
	// OutboundCommandAssignments: a variable given a literal value earlier
	// in the same command, in a place that always runs before its use, is
	// read instead of failing closed (D-60).
	OutboundCommandAssignments OutboundRule = "command_assignments"
	// OutboundGitConfigKeys: git -c with a key that changes neither where
	// git sends nor what it runs is read; any other key fails closed. D-60
	// added more such keys (gitSafeConfig).
	OutboundGitConfigKeys OutboundRule = "git_config_harmless_keys"
	// OutboundPendingWork: after a command of the same call changed what
	// is staged or committed, a commit or push also sends the work tree,
	// untracked files and, for a push, every branch.
	OutboundPendingWork OutboundRule = "pending_work_after_staging"
)

// OutboundBaselineRule describes one baseline rule.
type OutboundBaselineRule struct {
	ID OutboundRule
	// Loosens is set for a rule that reads more than the strict reading
	// would (D-60); switching it off would refuse more.
	Loosens bool
	Summary string
}

// OutboundBaseline lists the baseline rules in a fixed order. All are on.
var OutboundBaseline = []OutboundBaselineRule{
	{OutboundFailClosed, false, "refuse an outbound command that cannot be read in full"},
	{OutboundGHDefaultRepo, true, "a gh write without -R goes to the repo gh would use, not every remote"},
	{OutboundPlainVariables, true, "read $PWD, $USER and $HOME"},
	{OutboundCommandAssignments, true, "read literal assignments made earlier in the same command"},
	{OutboundGitConfigKeys, true, "read git -c with harmless keys"},
	{OutboundPendingWork, false, "after git add or commit in the same call, check the work tree and every branch"},
}
