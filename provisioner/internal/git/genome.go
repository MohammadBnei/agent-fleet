package git

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

// GenomeFile is one rendered context-pack file (docs/adr/0063).
type GenomeFile struct {
	Path string
	Body string
}

// The only paths a genome may hold. Nothing else, so no settings.json, .mcp.json
// or hook can reach the pod through it, and no name can climb out.
var genomePathRe = regexp.MustCompile(`^(CLAUDE\.md|\.claude/rules/[0-9]{2}-[a-z0-9][a-z0-9-]{0,62}\.md|\.claude/skills/[a-z0-9][a-z0-9-]{0,62}/SKILL\.md)$`)

// GenomePath is claude-home/<id>-genome: a sibling of the session's
// claude-home, not a child, because a child would also be reachable through
// the pod's read-write /claude-home mount. Mounted read-only at /genome.
func (m *Manager) GenomePath(sessionID string) string {
	return m.ClaudeHomePath(sessionID) + "-genome"
}

// WriteGenome replaces the session's genome with files. It is written into a
// fresh <id>-genome.new and swapped in by rename, so the directory a pod
// mounts is always one complete render. The swap runs before the pod exists,
// and a warm only happens with no pod running.
func (m *Manager) WriteGenome(sessionID string, files []GenomeFile) error {
	dir := m.GenomePath(sessionID)
	tmp, old := dir+".new", dir+".old"
	for _, f := range files {
		if !genomePathRe.MatchString(f.Path) {
			return fmt.Errorf("genome path %q is not allowed", f.Path)
		}
	}
	for _, p := range []string{tmp, old} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("clear %s: %w", p, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return fmt.Errorf("mkdir claude-home: %w", err)
	}
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return fmt.Errorf("mkdir genome: %w", err)
	}
	for _, f := range files {
		if err := writeGenomeFile(tmp, f); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	moved := false
	if _, err := os.Lstat(dir); err == nil {
		if err := os.Rename(dir, old); err != nil {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("move old genome aside: %w", err)
		}
		moved = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat genome: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		if moved {
			_ = os.Rename(old, dir)
		}
		return fmt.Errorf("swap genome in: %w", err)
	}
	return os.RemoveAll(old)
}

func writeGenomeFile(root string, f GenomeFile) error {
	path := filepath.Join(root, f.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", f.Path, err)
	}
	// O_EXCL|O_NOFOLLOW: the directory is fresh, so anything already there is
	// a duplicate path or a planted link, and neither is written through.
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", f.Path, err)
	}
	if _, err := fd.WriteString(f.Body); err != nil {
		_ = fd.Close()
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	return fd.Close()
}
