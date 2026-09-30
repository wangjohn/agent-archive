package agentskills

import (
	"bytes"
	"embed"
	"text/template"
)

// Destination is which family of agents reads a rendered skill file.
type Destination int

const (
	// Claude is Claude Code's file, in its configuration directory: it has
	// frontmatter of its own (permissions, invocation control).
	Claude Destination = iota
	// Shared is the one file Codex and Cursor read from ~/.agents/skills, so
	// it uses only frontmatter fields both accept.
	Shared
)

// A Skill is one installable skill. Render returns its file for one
// destination, running executable with AGENT_ARCHIVE_HOME=dataHome when
// dataHome is not "" (see commandLine). The file must carry marker, or
// setup would never own, replace, or remove it.
type Skill struct {
	// Name is the skill's directory name: "handoff", "agent-archive".
	Name string
	// Slash is true for a skill the person runs by name, as /handoff: setup
	// and status call it "/handoff". Any other skill is one the agent picks
	// up on its own, from its description, and is called "the agent-archive
	// skill".
	Slash bool
	// Summary says what the skill does, for setup's "Installed <title>,
	// which <summary>" line; empty leaves the clause out.
	Summary string
	Render  func(dest Destination, executable, dataHome string) []byte
}

// Title is how a sentence names the skill: "/handoff", or "the
// agent-archive skill".
func (s Skill) Title() string {
	if s.Slash {
		return "/" + s.Name
	}
	return "the " + s.Name + " skill"
}

// Label is how status names the skill called name, in a row or a warning
// ("The /handoff skill at ... is out of date"): "/handoff", or, for a skill
// the person does not run by name, "agent-archive". A name that is not in
// the Registry (a file an earlier release wrote) is labelled as a slash
// skill.
func Label(name string) string {
	for _, s := range Registry {
		if s.Name == name && !s.Slash {
			return name
		}
	}
	return "/" + name
}

// Registry lists every skill setup installs, in a stable order.
var Registry = []Skill{handoffSkill, archiveSkill}

// skillTemplates holds each skill's text, one directory per skill, so that a
// change to a skill's prose reviews as prose.
//
//go:embed skills
var skillTemplates embed.FS

// templateData is what a skill's template can name.
type templateData struct {
	// Marker is the line that makes the file setup's.
	Marker string
	// Claude is true for Claude Code's file, false for the shared one.
	Claude bool
	// Command is the command line to run, less its arguments: the
	// executable, quoted if it needs it, after AGENT_ARCHIVE_HOME=... for a
	// relocated installation.
	Command string
	// AllowCommand is true when Command is a plain path that a Claude Code
	// permission rule can name. A quoted path or a variable would not read
	// back as a plain YAML value or a rule that matches the command as run.
	AllowCommand bool
}

// newTemplateData is the data for a skill rendered to dest.
func newTemplateData(dest Destination, executable, dataHome string) templateData {
	command := commandLine(executable, dataHome)
	return templateData{Marker: marker, Claude: dest == Claude, Command: command, AllowCommand: command == executable}
}

// parseSkill parses the template skills/<name>/SKILL.md.tmpl. The templates
// are embedded, so a failure is a programming error, caught by the tests
// that render every skill.
func parseSkill(name string) *template.Template {
	return template.Must(template.New("SKILL.md.tmpl").Option("missingkey=error").ParseFS(skillTemplates, "skills/"+name+"/SKILL.md.tmpl"))
}

// render is the file a parsed skill template makes for data.
func render(t *template.Template, data templateData) []byte {
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		// Unreachable for a template the tests have rendered: its data has
		// every field it names.
		panic("agentskills: render " + t.Name() + ": " + err.Error())
	}
	return out.Bytes()
}

// handoffTemplate is /handoff's text: Claude Code's file takes the agent
// from $ARGUMENTS, is only for the person to invoke, and may run the one
// command without asking. Codex ($handoff) has no argument substitution, so
// the agent comes from the request itself, and neither it nor Cursor has
// disable-model-invocation, so the body repeats the description's guard.
var handoffTemplate = parseSkill("handoff")

var handoffSkill = Skill{
	Name:    "handoff",
	Slash:   true,
	Summary: "continues a session in another agent",
	Render: func(dest Destination, executable, dataHome string) []byte {
		return render(handoffTemplate, newTemplateData(dest, executable, dataHome))
	},
}

// archiveTemplate is the agent-archive skill's text: everything the agent
// needs is in the one file, since the installer renders one file per skill
// and Cursor may not load anything beside it. Claude Code's file lets the
// agent run `status` without asking, and nothing else (see the template).
var archiveTemplate = parseSkill("agent-archive")

var archiveSkill = Skill{
	Name:    "agent-archive",
	Summary: "lets your agents look up and pull in past sessions",
	Render: func(dest Destination, executable, dataHome string) []byte {
		return render(archiveTemplate, newTemplateData(dest, executable, dataHome))
	},
}
