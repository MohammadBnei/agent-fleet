// Package memory is the sidecar's memory tool (ADR-0086 §5): the single writer
// of a long-lived agent's memory repo. One git commit per write, tagged with the
// session and its set. A session whose set holds A writes only to the
// quarantine repo, which is never mounted into a session holding B or C.
package memory

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// ErrRefused: the set cannot write memory. Unknown or empty sets refuse (fail closed).
var ErrRefused = errors.New("memory write refused: the session's set holds no C and no A")

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Store writes under Root: agent-<Name>-memory and agent-<Name>-quarantine.
type Store struct {
	Root, Name, SessionID, Set string
	mu                         sync.Mutex // one writer; git's index is not concurrent
}

// Quarantined reports whether this session's writes go to quarantine.
func Quarantined(set string) bool { return strings.Contains(set, "A") }

// Dir is the repo a write from this session lands in, "" when it may not write.
func (s *Store) Dir() string {
	switch {
	case Quarantined(s.Set):
		return filepath.Join(s.Root, "agent-"+s.Name+"-quarantine")
	case strings.Contains(s.Set, "C"): // a memory write is a C effect
		return filepath.Join(s.Root, "agent-"+s.Name+"-memory")
	}
	return ""
}

// Write stores content at rel inside the repo and commits it. It returns the repo dir.
func (s *Store) Write(rel, content string) (string, error) {
	if !nameRe.MatchString(s.Name) {
		return "", fmt.Errorf("memory: bad agent name %q", s.Name)
	}
	dir := s.Dir()
	if dir == "" {
		return "", ErrRefused
	}
	clean := filepath.Clean(rel)
	if rel == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
		clean == "." || clean == ".git" || strings.HasPrefix(clean, ".git"+string(filepath.Separator)) {
		return "", fmt.Errorf("memory: bad path %q", rel)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := git(dir, "init", "-q"); err != nil {
			return "", err
		}
	}
	full := filepath.Join(dir, clean)
	// A symlink in the repo must not carry the write outside it.
	if r, err := filepath.EvalSymlinks(filepath.Dir(full)); err == nil {
		if root, _ := filepath.EvalSymlinks(dir); !strings.HasPrefix(r+string(filepath.Separator), root+string(filepath.Separator)) {
			return "", fmt.Errorf("memory: bad path %q", rel)
		}
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", err
	}
	set := s.Set
	if Quarantined(s.Set) {
		set = "neither" // quarantine writes are tagged neither B nor C
	}
	if err := git(dir, "add", "--", clean); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("memory: write %s\n\nSession: %s\nSet: %s\n", clean, s.SessionID, set)
	// --allow-empty: rewriting identical content still leaves its one commit.
	return dir, git(dir, "-c", "user.name=ukubi-agent", "-c", "user.email=noreply@bnei.dev", "commit", "-q", "--allow-empty", "-m", msg)
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, out)
	}
	return nil
}
