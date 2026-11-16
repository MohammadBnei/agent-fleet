package coreserver

// methodTags is docs/adr/0086 §1's tag for every method core or an executor serves: "B" (sensitive reads), "C"
// (effects core or an executor runs) or "" (neither). A method outside the session's stored trust set is refused
// with agent.rule_of_two. Every CoreService method is listed, "" included, so tagging a new RPC is a decision
// (TestMethodTagsCoverCoreService). Tool reads stay outside the rule: no read-taint (docs/adr/0082 §3).
var methodTags = map[string]string{
	// B: fleet-wide reads of private memory and logs.
	"/agentfleet.v1.CoreService/ViewLogs":      "B",
	"/agentfleet.v1.CoreService/SearchJournal": "B",
	// The executor's read path (thot-executor is read-only once ADR-0086 §3 moves the mutate verbs out). Enforced
	// when the sidecar's kubectl_read asks core first; the tag is the decision.
	"/agentfleet.v1.ExecutorService/Exec": "B",

	// C: effects core runs: memory writes, exposure, peer prompts, events, GitHub reviews.
	"/agentfleet.v1.CoreService/AppendJournal":  "C",
	"/agentfleet.v1.CoreService/Expose":         "C",
	"/agentfleet.v1.CoreService/Unexpose":       "C",
	"/agentfleet.v1.CoreService/RequestService": "C",
	"/agentfleet.v1.CoreService/PromptSession":  "C",
	"/agentfleet.v1.CoreService/PublishEvent":   "C",
	"/agentfleet.v1.CoreService/SubmitReview":   "C",

	// Neither: the session's own plumbing, files, cards (the way to ask the owner), pod lifecycle.
	"/agentfleet.v1.CoreService/ReportPodEvents":     "",
	"/agentfleet.v1.CoreService/SendMessage":         "",
	"/agentfleet.v1.CoreService/WaitForMessages":     "",
	"/agentfleet.v1.CoreService/AskUserQuestion":     "",
	"/agentfleet.v1.CoreService/GetSession":          "",
	"/agentfleet.v1.CoreService/SetPermissionMode":   "",
	"/agentfleet.v1.CoreService/UpdateSessionMeta":   "",
	"/agentfleet.v1.CoreService/SaveAgentSessionId":  "",
	"/agentfleet.v1.CoreService/PushToolTelemetry":   "",
	"/agentfleet.v1.CoreService/ExportSpans":         "",
	"/agentfleet.v1.CoreService/StreamHumanMessages": "",
	"/agentfleet.v1.CoreService/ListFiles":           "",
	"/agentfleet.v1.CoreService/GetFileUploadUrl":    "",
	"/agentfleet.v1.CoreService/GetFileDownloadUrl":  "",
	"/agentfleet.v1.CoreService/DeleteFile":          "",
	"/agentfleet.v1.CoreService/ListPeerSessions":    "",
	"/agentfleet.v1.CoreService/WaitForSessionState": "",
	"/agentfleet.v1.CoreService/ConsumeGrant":        "",
	"/agentfleet.v1.CoreService/DraftCard":           "",
}
