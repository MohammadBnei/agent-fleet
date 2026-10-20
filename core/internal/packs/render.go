// Package packs owns context packs (docs/adr/0063): reusable rules text and
// skills that every warm renders into the session's read-only /genome.
// This file is the pure half — validation and rendering, no database.
package packs

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	// Composed per target, ADR-0063 §2. Memory text loads on every turn;
	// skill bodies load only when the skill is used.
	MaxText   = 64 << 10
	MaxSkills = 256 << 10
)

var (
	// definitions' skillRe, length-capped: pack and skill names both become
	// path segments under /genome.
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// A CLAUDE.md-style memory import would pull in files the pack does not
	// hold (/proc/self/environ, agent-writable /workspace files). Refuse any
	// @ not glued to a letter or digit, so mail addresses still pass.
	importRe = regexp.MustCompile(`(^|[^A-Za-z0-9])@`)
	// Names the reviewed packs render under; a live pack cannot take them.
	reserved = map[string]bool{"fleet": true, "repo": true}
)

// Skill is stored without frontmatter. Render writes the frontmatter itself
// from Name and Description, so a live pack cannot set allowed-tools, hooks,
// model or any other key that widens authority.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// File renders the skill's SKILL.md. JSON quoting is a valid YAML scalar.
func (s Skill) File() string {
	d, _ := json.Marshal(s.Description)
	return "---\nname: " + s.Name + "\ndescription: " + string(d) + "\n---\n" + s.Body
}

// Version is one immutable pack version, live or reviewed. A reviewed one
// is identified by Ref, the commit it was read at, instead of Version.
type Version struct {
	Pack    string
	Version int
	Ref     string
	Text    string
	Skills  []Skill
}

func (v Version) id() string {
	if v.Ref != "" {
		return fmt.Sprintf("%s@%.7s", v.Pack, v.Ref)
	}
	return fmt.Sprintf("%s@%d", v.Pack, v.Version)
}

// File is one path under /genome and its content.
type File struct {
	Path string
	Body string
}

type Rendered struct {
	Files    []File
	Versions []string // "pack@version", what the warm rendered
	Skipped  []string // why something was left out
}

// Validate checks one version on its own. Render re-checks the composed sums.
func Validate(v Version) error {
	if err := checkPackName(v.Pack); err != nil {
		return err
	}
	if importRe.MatchString(v.Text) {
		return fmt.Errorf("text: @path imports are not allowed")
	}
	if len(v.Text) > MaxText {
		return fmt.Errorf("text is %d bytes, max %d", len(v.Text), MaxText)
	}
	seen := map[string]bool{}
	total := 0
	for _, s := range v.Skills {
		if !nameRe.MatchString(s.Name) {
			return fmt.Errorf("skill name %q must match %s", s.Name, nameRe)
		}
		if seen[s.Name] {
			return fmt.Errorf("skill %q appears twice", s.Name)
		}
		seen[s.Name] = true
		if importRe.MatchString(s.Body) || importRe.MatchString(s.Description) {
			return fmt.Errorf("skill %q: @path imports are not allowed", s.Name)
		}
		if strings.ContainsAny(s.Description, "\r\n") {
			return fmt.Errorf("skill %q: description must be one line", s.Name)
		}
		// ponytail: inline !`cmd` and fenced ```! both refused outright, until
		// dynamic context is shown not to bypass canUseTool.
		if strings.Contains(s.Body, "!`") || strings.Contains(s.Body, "`!") || strings.Contains(s.Body, "~!") {
			return fmt.Errorf("skill %q: ! dynamic context is not allowed", s.Name)
		}
		// Render writes the frontmatter; a second block in the body could be
		// read as the real one.
		if first := strings.TrimLeft(strings.TrimPrefix(s.Body, "\uFEFF"), " \t\r\n"); strings.HasPrefix(first, "---") {
			return fmt.Errorf("skill %q: body must not start with frontmatter; set the description instead", s.Name)
		}
		total += len(s.Body) + len(s.Description)
	}
	if total > MaxSkills {
		return fmt.Errorf("skills are %d bytes, max %d", total, MaxSkills)
	}
	return nil
}

func composedSize(vs []Version) (text, skills int) {
	for _, v := range vs {
		text += len(v.Text)
		for _, s := range v.Skills {
			skills += len(s.File())
		}
	}
	return text, skills
}

func checkPackName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("pack name %q must match %s", name, nameRe)
	}
	if reserved[name] {
		return fmt.Errorf("pack name %q is reserved for the reviewed pack", name)
	}
	return nil
}

// Render composes the files for one target. Reviewed packs (repo, fleet; nil
// when absent) always go in and claim their skill names first. Live packs
// follow in name order while they fit the caps; a live pack that fails
// validation or the text cap, or a live skill that would shadow a placed one
// or overflow the skills cap, is left out and named in Skipped.
//
// Rules files are numbered so that later memory, which has precedence, is the
// reviewed text: live 20-<pack>, then 80-repo, then 90-fleet (ADR-0063 §6).
func Render(live []Version, repo, fleet *Version) Rendered {
	var r Rendered
	text, skillBytes := 0, 0
	owner := map[string]string{} // skill name -> pack that placed it
	var rules, skills []File
	skip := func(v Version, format string, args ...any) {
		r.Skipped = append(r.Skipped, v.id()+": "+fmt.Sprintf(format, args...))
	}

	add := func(v Version, rulesName string, reviewed bool) {
		if !reviewed {
			if err := Validate(v); err != nil {
				skip(v, "%v", err)
				return
			}
			if text+len(v.Text) > MaxText {
				skip(v, "text over the %d byte cap", MaxText)
				return
			}
		}
		if v.Text != "" {
			text += len(v.Text)
			rules = append(rules, File{Path: ".claude/rules/" + rulesName + ".md", Body: v.Text})
		}
		partial := ""
		for _, s := range v.Skills {
			body := s.File()
			if reviewed {
				body = s.Body // a reviewed SKILL.md is used as written, frontmatter included
			}
			switch p, taken := owner[s.Name]; {
			case !nameRe.MatchString(s.Name):
				skip(v, "skill %q: name must match %s", s.Name, nameRe)
			case taken:
				skip(v, "skill %s: name already used by %s", s.Name, p)
			case !reviewed && skillBytes+len(body) > MaxSkills:
				skip(v, "skill %s: over the %d byte skills cap", s.Name, MaxSkills)
			default:
				owner[s.Name] = v.Pack
				skillBytes += len(body)
				skills = append(skills, File{Path: ".claude/skills/" + s.Name + "/SKILL.md", Body: body})
				continue
			}
			partial = " (partial)"
		}
		tag := ""
		if reviewed {
			tag = "reviewed:"
		}
		r.Versions = append(r.Versions, tag+v.id()+partial)
	}

	if fleet != nil {
		add(*fleet, "90-fleet", true)
	}
	if repo != nil {
		add(*repo, "80-repo", true)
	}
	sorted := append([]Version(nil), live...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Pack < sorted[j].Pack })
	for _, v := range sorted {
		add(v, "20-"+v.Pack, false)
	}

	sort.Slice(rules, func(i, j int) bool { return rules[i].Path < rules[j].Path })
	r.Files = append(rules, skills...)
	return r
}
