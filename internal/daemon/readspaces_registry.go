package daemon

import orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"

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
	// spaceRecords: orch records other than memory (PRs, tasks, events,
	// docs and sources registries, logs); filtering them for agent callers
	// is SP-026's, or the follow-up its why names.
	spaceRecords = "records"
	// spaceNone: returns no stored content: a write, a control action, or
	// orchd's own status.
	spaceNone = "none"
)

// readSpaceDecisions is the space decision of every RPC, by full method
// name.
var readSpaceDecisions = map[string]spaceDecision{
	orchdv1.Orchd_Search_FullMethodName:      {spaceFiltered, "memory search, read only from the caller's readable spaces' collections"},
	orchdv1.Orchd_ListSources_FullMethodName: {spaceFiltered, "memory inventory, listed only from the caller's readable spaces"},
	orchdv1.Orchd_GetDocument_FullMethodName: {spaceFiltered, "one stored source, read only from the caller's readable spaces; NotFound otherwise"},
	orchdv1.Orchd_ListDocs_FullMethodName:    {spaceFiltered, "synced docs in the caller's readable spaces, with only their owners and repos"},

	orchdv1.Orchd_DiffSpaces_FullMethodName:         {spaceOperator, "the whole spaces policy"},
	orchdv1.Orchd_ApplySpaces_FullMethodName:        {spaceOperator, "applies the policy"},
	orchdv1.Orchd_ListSpaces_FullMethodName:         {spaceOperator, "every space's orgs, roots and checkouts"},
	orchdv1.Orchd_ScanSpaces_FullMethodName:         {spaceOperator, "folders found on disk"},
	orchdv1.Orchd_ReplaySpaces_FullMethodName:       {spaceOperator, "any session's command log"},
	orchdv1.Orchd_VerifyToolLog_FullMethodName:      {spaceOperator, "the command log's chains"},
	orchdv1.Orchd_SummarizeToolLog_FullMethodName:   {spaceOperator, "every space's activity"},
	orchdv1.Orchd_SetEnforcement_FullMethodName:     {spaceOperator, "switches a layer"},
	orchdv1.Orchd_SetSandboxSetting_FullMethodName:  {spaceOperator, "changes the sandbox setting"},
	orchdv1.Orchd_ChangeOutboundRule_FullMethodName: {spaceOperator, "changes an outbound rule"},
	orchdv1.Orchd_Relabel_FullMethodName:            {spaceOperator, "relabels the RAG store; its report counts chunks per space"},
	orchdv1.Orchd_RetagSource_FullMethodName:        {spaceOperator, "moves a source between spaces; its answer names the space that held it"},
	orchdv1.Orchd_ListUnlocks_FullMethodName:        {spaceOperator, "pending and active unlocks with their command lines; Orch.app only (SP-077)"},
	orchdv1.Orchd_ConfirmUnlock_FullMethodName:      {spaceOperator, "confirms or denies an unlock; Orch.app only (SP-077)"},
	orchdv1.Orchd_RequestUnlock_FullMethodName:      {spaceOwn, "the caller's own unlock request (SP-077)"},
	orchdv1.Orchd_ProveUnlock_FullMethodName:        {spaceOwn, "the caller's own unlock, without its code (SP-077)"},
	orchdv1.Orchd_Lock_FullMethodName:               {spaceNone, "ends unlocks; answers a count (SP-077)"},

	orchdv1.Orchd_ListToolEvents_FullMethodName:    {spaceOwn, "the caller's own command log; the operator's is everyone's (INV-12)"},
	orchdv1.Orchd_ExplainPlacement_FullMethodName:  {spaceOwn, "answers agents only about public placements"},
	orchdv1.Orchd_GetEnforcement_FullMethodName:    {spaceOwn, "the switches' states, which every start digest names (INV-16)"},
	orchdv1.Orchd_PinSession_FullMethodName:        {spaceOwn, "the caller's own pin"},
	orchdv1.Orchd_EndSession_FullMethodName:        {spaceOwn, "ends the caller's own pin"},
	orchdv1.Orchd_ListOutboundRules_FullMethodName: {spaceOwn, "rule summaries without values; the operator also sees who set them"},

	orchdv1.Orchd_ListPRs_FullMethodName:               {spaceRecords, "cached PRs (SP-026)"},
	orchdv1.Orchd_ListGitHubItems_FullMethodName:       {spaceRecords, "cached GitHub items (SP-026)"},
	orchdv1.Orchd_GetPRDetail_FullMethodName:           {spaceRecords, "PR titles and bodies (SP-026; detail cache RC-351)"},
	orchdv1.Orchd_ListPRReviews_FullMethodName:         {spaceRecords, "PR review verdicts (SP-026)"},
	orchdv1.Orchd_ListTasks_FullMethodName:             {spaceRecords, "the task ledger (SP-026)"},
	orchdv1.Orchd_GetTask_FullMethodName:               {spaceRecords, "one task's record and result (SP-026)"},
	orchdv1.Orchd_WatchTasks_FullMethodName:            {spaceRecords, "task changes (SP-026)"},
	orchdv1.Orchd_GetTaskActivity_FullMethodName:       {spaceRecords, "a task's transcript (SP-026)"},
	orchdv1.Orchd_GetPRLanding_FullMethodName:          {spaceRecords, "a PR landing (SP-026)"},
	orchdv1.Orchd_ListPRLandings_FullMethodName:        {spaceRecords, "PR landings (SP-026)"},
	orchdv1.Orchd_ListSessions_FullMethodName:          {spaceRecords, "sessions with their spaces and directories (SP-026)"},
	orchdv1.Orchd_GetGitHubIdentity_FullMethodName:     {spaceRecords, "the GitHub logins (RC-700)"},
	orchdv1.Orchd_ListGitHubScopes_FullMethodName:      {spaceRecords, "monitored GitHub scopes (RC-700)"},
	orchdv1.Orchd_ListGitHubRepos_FullMethodName:       {spaceRecords, "repos GitHub lists (RC-700)"},
	orchdv1.Orchd_GetDocsSync_FullMethodName:           {spaceRecords, "the docs sync's progress, naming an owner and repo (RC-700)"},
	orchdv1.Orchd_ListDocsOwners_FullMethodName:        {spaceRecords, "docs owners and their repo choices (RC-700)"},
	orchdv1.Orchd_ListDocsOwnerRepos_FullMethodName:    {spaceRecords, "an owner's repos on GitHub (RC-700)"},
	orchdv1.Orchd_ListRegisteredSources_FullMethodName: {spaceRecords, "registered sources' URIs and folders (RC-700)"},
	orchdv1.Orchd_TailLogs_FullMethodName:              {spaceRecords, "orchd's log lines, which name source URIs (RC-700)"},
	orchdv1.Orchd_GetConfig_FullMethodName:             {spaceRecords, "orch's settings (RC-700)"},
	orchdv1.Orchd_ListSettings_FullMethodName:          {spaceRecords, "managed harness settings (RC-700)"},

	orchdv1.Orchd_Ping_FullMethodName:                     {spaceNone, "orchd's version"},
	orchdv1.Orchd_Status_FullMethodName:                   {spaceNone, "orchd's subsystems"},
	orchdv1.Orchd_GetSystemMetrics_FullMethodName:         {spaceNone, "orchd's resource use"},
	orchdv1.Orchd_GetSafetyNet_FullMethodName:             {spaceNone, "cc-safety-net's install state"},
	orchdv1.Orchd_RecommendModel_FullMethodName:           {spaceNone, "a model for a task kind"},
	orchdv1.Orchd_Ingest_FullMethodName:                   {spaceNone, "a write, placed by orchd (SP-023)"},
	orchdv1.Orchd_DeleteSource_FullMethodName:             {spaceNone, "a delete; its count is RC-701's"},
	orchdv1.Orchd_SummarizeSession_FullMethodName:         {spaceNone, "a write"},
	orchdv1.Orchd_BackfillSessions_FullMethodName:         {spaceNone, "a write"},
	orchdv1.Orchd_MigrateHybrid_FullMethodName:            {spaceNone, "a migration report of counts"},
	orchdv1.Orchd_DropBackup_FullMethodName:               {spaceNone, "a migration report of counts"},
	orchdv1.Orchd_ReindexKeywords_FullMethodName:          {spaceNone, "a reindex report of counts"},
	orchdv1.Orchd_Dispatch_FullMethodName:                 {spaceNone, "queues a task"},
	orchdv1.Orchd_RateTask_FullMethodName:                 {spaceNone, "rates a task"},
	orchdv1.Orchd_RetryTask_FullMethodName:                {spaceNone, "retries a task"},
	orchdv1.Orchd_CancelTask_FullMethodName:               {spaceNone, "cancels a task"},
	orchdv1.Orchd_SaveSource_FullMethodName:               {spaceNone, "registers a source"},
	orchdv1.Orchd_RemoveSource_FullMethodName:             {spaceNone, "removes a source"},
	orchdv1.Orchd_RefreshSource_FullMethodName:            {spaceNone, "refreshes a source"},
	orchdv1.Orchd_SyncDocs_FullMethodName:                 {spaceNone, "starts a docs sync"},
	orchdv1.Orchd_SaveDocsOwner_FullMethodName:            {spaceNone, "saves a docs owner"},
	orchdv1.Orchd_RemoveDocsOwner_FullMethodName:          {spaceNone, "removes a docs owner"},
	orchdv1.Orchd_PurgeDocs_FullMethodName:                {spaceNone, "deletes synced docs"},
	orchdv1.Orchd_SetGitHubScope_FullMethodName:           {spaceNone, "changes GitHub monitoring"},
	orchdv1.Orchd_RequestGitHubSweep_FullMethodName:       {spaceNone, "asks for a sweep"},
	orchdv1.Orchd_WatchPRReview_FullMethodName:            {spaceNone, "starts a review watch"},
	orchdv1.Orchd_SetPRReviewUpdateInPlace_FullMethodName: {spaceNone, "changes a review watch"},
	orchdv1.Orchd_StopPRReview_FullMethodName:             {spaceNone, "stops a review watch"},
	orchdv1.Orchd_LandPR_FullMethodName:                   {spaceNone, "pushes proposed commits"},
	orchdv1.Orchd_UndoPRLanding_FullMethodName:            {spaceNone, "undoes a landing"},
	orchdv1.Orchd_DiscardPRLanding_FullMethodName:         {spaceNone, "discards a landing"},
	orchdv1.Orchd_SetConfig_FullMethodName:                {spaceNone, "changes a setting"},
	orchdv1.Orchd_RemoveLegacyConfig_FullMethodName:       {spaceNone, "removes config.yaml"},
	orchdv1.Orchd_Restart_FullMethodName:                  {spaceNone, "restarts subsystems"},
	orchdv1.Orchd_InstallSafetyNet_FullMethodName:         {spaceNone, "installs cc-safety-net"},
	orchdv1.Orchd_SetSettingEnabled_FullMethodName:        {spaceNone, "changes a harness setting"},
	orchdv1.Orchd_AddSetting_FullMethodName:               {spaceNone, "adds a harness setting"},
	orchdv1.Orchd_RemoveSetting_FullMethodName:            {spaceNone, "removes a harness setting"},
	orchdv1.Orchd_SetSpacesDraft_FullMethodName:           {spaceNone, "saves a draft and its problems"},
	orchdv1.Orchd_RecordToolEvent_FullMethodName:          {spaceNone, "records a tool event"},
}
