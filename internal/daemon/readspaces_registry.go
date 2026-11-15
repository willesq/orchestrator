package daemon

// spaceDecision says how an RPC keeps the content it returns within the
// caller's spaces (SP-025.2). Every RPC orchd serves has one, as every RPC
// has a place in taskCallable or operatorActions, so a new RPC cannot
// return stored content without someone deciding how it is scoped.
type spaceDecision struct {
	// kind is one of the spaceDecision kinds below.
	kind string
	// why says what the RPC returns and how it is kept in bounds.
	why string
}

// Space decision kinds.
const (
	// spaceFiltered: memory reads filtered to the caller's readable spaces
	// inside orchd (readscope.go).
	spaceFiltered = "filtered"
	// spaceOperator: refused to every caller but the operator
	// (refuseNonOperator; under a spaces policy only Orch.app, D-71), so
	// no session or task reads it.
	spaceOperator = "operator"
	// spaceOwn: answers a caller only about itself, or about public
	// content.
	spaceOwn = "own"
	// spaceRecords: orch records other than memory (PRs, tasks, docs and
	// sources registries, logs), kept within the caller's readable spaces
	// by each record's space (records_scope.go, SP-026), or the follow-up
	// its why names.
	spaceRecords = "records"
	// spaceNone: returns no stored content: a write, a control action, or
	// orchd's own status.
	spaceNone = "none"
)

// readSpaceDecisions is the space decision of every RPC, by full method
// name. Each RPC registers its decision, with its caller class, from the
// file of its handler through registerRPCs (rpc_registry.go, SP-081).
var readSpaceDecisions = map[string]spaceDecision{}
