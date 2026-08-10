package k8s

import "testing"

func TestScopedName_Deterministic(t *testing.T) {
	n1, err := scopedName("dream-analyst", "abc-123-def", ScopeModeTaskScoped)
	if err != nil {
		t.Fatalf("scopedName: %v", err)
	}
	n2, err := scopedName("dream-analyst", "abc-123-def", ScopeModeTaskScoped)
	if err != nil {
		t.Fatalf("scopedName: %v", err)
	}
	if n1 != n2 {
		t.Errorf("scopedName not deterministic: %q vs %q", n1, n2)
	}
	if n1 != "task_"+shortID("abc-123-def") {
		t.Errorf("scopedName = %q, want task_%s", n1, shortID("abc-123-def"))
	}

	// A different task ID must produce a different name — otherwise two
	// unrelated tasks would silently share one database.
	n3, _ := scopedName("dream-analyst", "different-task", ScopeModeTaskScoped)
	if n1 == n3 {
		t.Error("different task IDs produced the same scoped name")
	}
}

func TestScopedName_RepoScopedIgnoresTaskID(t *testing.T) {
	n1, err := scopedName("dream-analyst", "task-a", ScopeModeRepoScoped)
	if err != nil {
		t.Fatalf("scopedName: %v", err)
	}
	n2, err := scopedName("dream-analyst", "task-b", ScopeModeRepoScoped)
	if err != nil {
		t.Fatalf("scopedName: %v", err)
	}
	if n1 != n2 {
		t.Errorf("repo-scoped name should ignore task id: %q vs %q", n1, n2)
	}
}

func TestScopedName_TaskScopedRequiresTaskID(t *testing.T) {
	if _, err := scopedName("dream-analyst", "", ScopeModeTaskScoped); err == nil {
		t.Error("expected error for empty task id in task-scoped mode")
	}
}

func TestScopedName_UnknownMode(t *testing.T) {
	if _, err := scopedName("dream-analyst", "task-a", "not-a-real-mode"); err == nil {
		t.Error("expected error for unknown scope mode")
	}
}

func TestDerivePassword_DeterministicAndDistinct(t *testing.T) {
	p1 := derivePassword("admin-secret", "task_abc123")
	p2 := derivePassword("admin-secret", "task_abc123")
	if p1 != p2 {
		t.Errorf("derivePassword not deterministic: %q vs %q", p1, p2)
	}
	p3 := derivePassword("admin-secret", "task_different")
	if p1 == p3 {
		t.Error("different names produced the same password")
	}
	p4 := derivePassword("different-admin-secret", "task_abc123")
	if p1 == p4 {
		t.Error("different admin secrets produced the same password")
	}
	if len(p1) != 32 {
		t.Errorf("derivePassword length = %d, want 32", len(p1))
	}
}

func TestSanitizeForIdentifier(t *testing.T) {
	cases := map[string]string{
		"dream-analyst": "dream_analyst",
		"Agent-Fleet":   "agent_fleet",
		"already_clean": "already_clean",
	}
	for in, want := range cases {
		if got := sanitizeForIdentifier(in); got != want {
			t.Errorf("sanitizeForIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPgQuoteIdentAndEscapeLiteral(t *testing.T) {
	if got := pgQuoteIdent(`weird"name`); got != `"weird""name"` {
		t.Errorf("pgQuoteIdent = %q", got)
	}
	if got := pgEscapeLiteral(`weird'pass`); got != `weird''pass` {
		t.Errorf("pgEscapeLiteral = %q", got)
	}
}
