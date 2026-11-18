package coreserver

// methodTags is docs/adr/0086 §1's tag for every method core serves: "B" (sensitive reads), "C"
// (effects core runs) or "" (neither). A method outside the session's stored trust set is refused
// with agent.rule_of_two. Every CoreService method is listed, "" included, so tagging a new RPC is a decision
// (TestMethodTagsCoverCoreService). Tool reads stay outside the rule: no read-taint (docs/adr/0082 §3).
var methodTags = map[string]string{
	// B: fleet-wide reads of private memory and logs.
	"/agentfleet.v1.CoreService/ViewLogs":      "B",
	"/agentfleet.v1.CoreService/QueryMetrics":  "B", // read-only PromQL relay (ADR-0084 §6)
	"/agentfleet.v1.CoreService/SearchJournal": "B",
	// ADR-0084 §5: typed fields only, but the owner's alert history is private, so it is a read of B.
	"/agentfleet.v1.CoreService/ListResponderCards": "B",
	// ADR-0086 §2: the sidecar's kubectl_read asks core before it runs kubectl.
	"/agentfleet.v1.CoreService/AuthorizeKubeRead": "B",

	// C: effects core runs: memory writes, exposure, peer prompts, events, GitHub reviews.
	"/agentfleet.v1.CoreService/AppendJournal":        "A|C", // a set holding A writes quarantined rows (ADR-0086 §5)
	"/agentfleet.v1.CoreService/AuthorizeMemoryWrite": "A|C", // A: quarantine repo; C: memory repo; B alone: refused
	"/agentfleet.v1.CoreService/Expose":               "C",
	"/agentfleet.v1.CoreService/Unexpose":             "C",
	"/agentfleet.v1.CoreService/RequestService":       "C",
	"/agentfleet.v1.CoreService/PromptSession":        "C",
	"/agentfleet.v1.CoreService/PublishEvent":         "C",
	"/agentfleet.v1.CoreService/SubmitReview":         "C",
	"/agentfleet.v1.CoreService/SpawnWorker":          "C", // a run starts and acts (ADR-0084 §6)
	"/agentfleet.v1.CoreService/OpenIssue":            "C",
	"/agentfleet.v1.CoreService/Speak":                "C", // a line to the owner's browser (ADR-0084 §7)

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
	"/agentfleet.v1.CoreService/WaitForSpawn":        "", // reads the character's own run, which it started
	"/agentfleet.v1.CoreService/ConsumeGrant":        "",
	"/agentfleet.v1.CoreService/DraftCard":           "",
}

// ponytail: ExecutorService/Exec is not tagged. Core never serves it; the executor authenticates with the shared
// bearer and sees no lease or trust set, so nothing enforces a tag there. Until the executor slice (ADR-0086 §3)
// moves the mutate verbs out and adds a core check, Exec's mutating path is B+C and runs unenforced.
