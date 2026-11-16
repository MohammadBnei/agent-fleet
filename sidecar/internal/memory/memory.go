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

// MaxContent caps one write: the repos live on the fleet-wide shared volume.
const MaxContent = 256 << 10

// pathRe: no control characters, spaces or newlines in a memory path.
var pathRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

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

// WriteAs is Write after core's word on where the write goes (ADR-0086 §5). The pod's own set must agree: a
// disagreement means a stale pod or a bug, and the write is refused.
func (s *Store) WriteAs(quarantine bool, rel, content string) (string, error) {
	if quarantine != Quarantined(s.Set) {
		return "", ErrRefused
	}
	return s.Write(rel, content)
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
	if len(content) > MaxContent {
		return "", fmt.Errorf("memory: content is %d bytes, over the %d byte cap", len(content), MaxContent)
	}
	clean := filepath.Clean(rel)
	// The path lands in the commit message and the tree: a strict charset keeps a
	// newline from forging a Session/Set line, and no component may be dot-led
	// ".git" or "..".
	if !pathRe.MatchString(rel) || clean == "." || filepath.IsAbs(clean) {
		return "", fmt.Errorf("memory: bad path %q", rel)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." || strings.EqualFold(part, ".git") {
			return "", fmt.Errorf("memory: bad path %q", rel)
		}
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
	// No existing component of the path may be a symlink, the last one included.
	cur := dir
	for _, part := range strings.Split(clean, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("memory: bad path %q", rel)
		}
	}
	full := filepath.Join(dir, clean)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	prev, readErr := os.ReadFile(full)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", err
	}
	set := s.Set
	if Quarantined(s.Set) {
		set = "neither" // quarantine writes are tagged neither B nor C
	}
	msg := fmt.Sprintf("memory: write %s\n\nSession: %s\nSet: %s\n", clean, s.SessionID, set)
	// Commit only this path, so nothing staged earlier rides along under this
	// write's tags. --allow-empty: rewriting identical content still leaves its one commit.
	err := git(dir, "add", "--", clean)
	if err == nil {
		err = git(dir, "-c", "user.name=ukubi-agent", "-c", "user.email=noreply@bnei.dev", "commit", "-q", "--allow-empty", "-m", msg, "--", clean)
	}
	if err != nil {
		// Leave no content without its provenance commit.
		_ = git(dir, "reset", "-q", "--", clean)
		if readErr == nil {
			_ = os.WriteFile(full, prev, 0o644)
		} else {
			_ = os.Remove(full)
		}
		return "", err
	}
	return dir, nil
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, out)
	}
	return nil
}
