package archive

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// HandoffVersion identifies the Handoff document shape and the markdown
// layout RenderHandoffMarkdown produces from it.
const HandoffVersion = 1

// DefaultHandoffMaxBytes is the default output budget, about 30k tokens.
const DefaultHandoffMaxBytes = 120_000

// handoffKeptExchanges and handoffKeptSteps bound the protected tail the
// budget never trims: steps in the last three exchanges, but at most the last
// twenty steps, so a single long exchange can still be trimmed.
const (
	handoffKeptExchanges = 3
	handoffKeptSteps     = 20
)

const (
	handoffAssistantTextCap = 300
	handoffPromptCap        = 2000
	handoffSummaryCap       = 200
	handoffAgentPromptCap   = 120
)

// HandoffOptions controls how much of each tool result BuildHandoff keeps
// before any budget is applied.
type HandoffOptions struct {
	// ToolResultLines is how many lines of a result are kept, split between
	// its head and tail. Zero means 12.
	ToolResultLines int
	// ToolResultBytes caps a result after line trimming, split between head
	// and tail. Zero means 2,000.
	ToolResultBytes int
	// Source names where the bundle came from ("local", "archive", "file").
	Source string
	// StartedAt and LastActivityAt are fallbacks the caller knows (a
	// registration's start, a transcript's modification time), used only
	// when no retained record carries a timestamp. Neither is ever inferred
	// from the time the handoff is built.
	StartedAt      time.Time
	LastActivityAt time.Time
}

func (o HandoffOptions) resultLines() int {
	if o.ToolResultLines > 0 {
		return o.ToolResultLines
	}
	return 12
}

func (o HandoffOptions) resultBytes() int {
	if o.ToolResultBytes > 0 {
		return o.ToolResultBytes
	}
	return 2000
}

// Handoff is a filtered session arranged for another coding agent to continue
// from. It is built only from a filtered SourceBundle, never from a raw
// transcript, so it carries no more than the archive already retains.
type Handoff struct {
	Version   int              `json:"handoff_version"`
	Session   HandoffSession   `json:"session"`
	Workspace HandoffWorkspace `json:"workspace"`
	// LeftOff is the last assistant text in the session.
	LeftOff      string            `json:"left_off,omitempty"`
	Plan         []HandoffPlanItem `json:"plan,omitempty"`
	FilesTouched []string          `json:"files_touched,omitempty"`
	Exchanges    []HandoffExchange `json:"exchanges"`
	// ToolResultsUnavailable is true when the harness records no tool
	// results at all (Cursor), so their absence is stated once.
	ToolResultsUnavailable bool             `json:"tool_results_unavailable,omitempty"`
	Gaps                   []HandoffGap     `json:"capture_gaps,omitempty"`
	Elisions               []HandoffElision `json:"elisions,omitempty"`
	// FullRecordPath, when set, names where the untrimmed rendering was
	// saved. It is rendered only when Elisions is non-empty.
	FullRecordPath string `json:"full_record_path,omitempty"`
}

type HandoffSession struct {
	ArchiveSessionID string        `json:"archive_session_id,omitempty"`
	NativeSessionID  string        `json:"native_session_id,omitempty"`
	Harness          string        `json:"harness"`
	HarnessVersion   string        `json:"harness_version,omitempty"`
	Models           []string      `json:"models,omitempty"`
	StartedAt        *time.Time    `json:"started_at,omitempty"`
	LastActivityAt   *time.Time    `json:"last_activity_at,omitempty"`
	State            MetadataState `json:"state,omitempty"`
	TurnOutcome      TurnOutcome   `json:"turn_outcome,omitempty"`
	Source           string        `json:"source,omitempty"`
}

// HandoffWorkspace is what the transcript recorded about where the work
// happened. Directory is a base name only.
type HandoffWorkspace struct {
	Directory string `json:"directory,omitempty"`
	Branch    string `json:"branch,omitempty"`
}

type HandoffPlanItem struct {
	Text   string `json:"text"`
	Status string `json:"status,omitempty"`
}

// HandoffExchange is one human prompt and everything the agent did before
// the next one. A leading exchange with an empty prompt holds agent activity
// that preceded the first prompt.
type HandoffExchange struct {
	Prompt          string        `json:"prompt,omitempty"`
	PromptTruncated bool          `json:"prompt_truncated,omitempty"`
	Timestamp       string        `json:"timestamp,omitempty"`
	Steps           []HandoffStep `json:"steps,omitempty"`
}

// HandoffStep is assistant text, a tool call, a shell command the person ran
// directly, the summary Claude Code wrote when the session was compacted, or —
// once the budget has collapsed tool calls — a count of them such as
// "14 tool calls: Bash ×9, Read ×5".
type HandoffStep struct {
	Kind          string           `json:"kind"` // "text", "tool", "shell", "summary", or "collapsed"
	Text          string           `json:"text,omitempty"`
	TextTruncated bool             `json:"text_truncated,omitempty"`
	Tool          *HandoffToolCall `json:"tool,omitempty"`
}

type HandoffToolCall struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// Result is the trimmed retained output; ResultLines and ResultBytes
	// describe the whole retained output.
	Result        string `json:"result,omitempty"`
	ResultLines   int    `json:"result_lines,omitempty"`
	ResultBytes   int    `json:"result_bytes,omitempty"`
	ResultOmitted bool   `json:"result_omitted,omitempty"`
}

type HandoffGap struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// HandoffElision records one budget step: what it removed and from which
// exchanges (1-based, inclusive).
type HandoffElision struct {
	Kind  string `json:"kind"`
	First int    `json:"first_exchange"`
	Last  int    `json:"last_exchange"`
	Count int    `json:"count"`
}

const (
	HandoffElisionToolOutput    = "tool_output"
	HandoffElisionToolCalls     = "tool_calls"
	HandoffElisionAssistantText = "assistant_text"
	HandoffElisionPromptText    = "prompt_text"
)

// BuildHandoff arranges a filtered bundle for handoff without any budget.
// metadata is optional; when present it supplies lifecycle state and models a
// hook reported.
func BuildHandoff(bundle SourceBundle, metadata *Metadata, opts HandoffOptions) (Handoff, error) {
	view, err := ParseNormalized(bundle)
	if err != nil {
		return Handoff{}, err
	}
	h := Handoff{Version: HandoffVersion, Exchanges: []HandoffExchange{}}
	h.Session = HandoffSession{
		ArchiveSessionID: bundle.ArchiveSessionID,
		NativeSessionID:  bundle.NativeSessionID,
		Harness:          bundle.Capture.Harness.Name,
		HarnessVersion:   bundle.Capture.Harness.Version,
		Source:           opts.Source,
	}
	h.Workspace = recordedWorkspace(bundle)
	h.ToolResultsUnavailable = bundle.Capture.Harness.Name == "cursor" && len(view.ToolResults) == 0 && len(view.ToolCalls) > 0

	seenModel := map[string]bool{}
	addModel := func(model string) {
		// Claude Code labels messages it writes itself "<synthetic>".
		if model != "" && !strings.HasPrefix(model, "<") && !seenModel[model] {
			seenModel[model] = true
			h.Session.Models = append(h.Session.Models, model)
		}
	}
	var first, last time.Time
	for _, turn := range view.Turns {
		addModel(turn.Model)
		addModel(turn.ResponseModel)
		if t, err := time.Parse(time.RFC3339Nano, turn.Timestamp); err == nil {
			if first.IsZero() || t.Before(first) {
				first = t
			}
			if t.After(last) {
				last = t
			}
		}
	}
	if metadata != nil {
		for _, model := range metadata.Models {
			addModel(model.Attributes["gen_ai.response.model"])
			addModel(model.Attributes["gen_ai.request.model"])
		}
		h.Session.State, h.Session.TurnOutcome = metadata.State, metadata.TurnOutcome
		if first.IsZero() {
			first = metadata.StartedAt
		}
		// A published capture is taken from the transcript as it stood, so
		// its capture time bounds the last activity it shows. A bundle built
		// just now has no such meaning, which is why CapturedAt itself is
		// never used.
		if last.IsZero() {
			last = metadata.CapturedAt
		}
	}
	if first.IsZero() {
		first = opts.StartedAt
	}
	if last.IsZero() {
		last = opts.LastActivityAt
	}
	if !first.IsZero() {
		t := first.UTC()
		h.Session.StartedAt = &t
	}
	if !last.IsZero() {
		t := last.UTC()
		h.Session.LastActivityAt = &t
	}

	h.Gaps = countGaps(bundle.Capture.Gaps)
	if len(bundle.NativeRecords) == 0 && len(bundle.NativeText) > 0 {
		// A Cursor text transcript: role sections, no records to walk.
		h.Exchanges, h.LeftOff = textTranscriptExchanges(bundle.NativeText, opts)
		h.ToolResultsUnavailable = false
		return h, nil
	}
	events := handoffEvents(view)
	files := fileSet{}
	root := workspaceRoot(bundle)
	var current *HandoffExchange
	flush := func() {
		if current != nil && (current.Prompt != "" || len(current.Steps) > 0) {
			h.Exchanges = append(h.Exchanges, *current)
		}
	}
	current = &HandoffExchange{}
	for _, event := range events {
		if event.turn != nil {
			turn := event.turn
			switch turn.Kind {
			case TurnKindHumanPrompt:
				flush()
				current = &HandoffExchange{Prompt: cleanPrompt(turn.Text), Timestamp: turn.Timestamp}
			case TurnKindAssistant:
				if text := strings.TrimSpace(turn.Text); text != "" {
					current.Steps = append(current.Steps, HandoffStep{Kind: "text", Text: text})
					h.LeftOff = text
				}
			case TurnKindShellCommand:
				if command := strings.TrimSpace(stripHarnessTag(turn.Text, "bash-input")); command != "" {
					current.Steps = append(current.Steps, HandoffStep{Kind: "shell", Text: command})
				}
			case TurnKindCompactSummary:
				// After /compact the agent worked from this model-written
				// summary rather than the turns before it, so the receiving
				// agent should see it where it happened.
				if text := strings.TrimSpace(turn.Text); text != "" {
					current.Steps = append(current.Steps, HandoffStep{Kind: "summary", Text: text})
				}
			}
			continue
		}
		call := event.call
		raw := call.raw
		name := call.Name
		if name == "" {
			name = firstString(raw, "type")
		}
		if name == "" {
			name = "tool"
		}
		tool := &HandoffToolCall{Name: name, Summary: toolSummary(name, call.Input, raw, root)}
		if call.Name == "" && tool.Summary == "" {
			// A completion event with neither a tool name nor arguments (a
			// Codex Extension item) says only that something finished.
			continue
		}
		if call.IsError != nil {
			tool.IsError = *call.IsError
		}
		if call.ResultRecordIndex != nil {
			text := call.resultText
			tool.ResultBytes = len(text)
			tool.ResultLines = lineCount(text)
			tool.Result = trimResult(text, opts.resultLines(), opts.resultBytes())
		}
		for _, file := range touchedFiles(name, call.Input, raw) {
			files.add(relativeTo(file, root))
		}
		if plan := planItems(name, call.Input); plan != nil {
			h.Plan = plan
		}
		current.Steps = append(current.Steps, HandoffStep{Kind: "tool", Tool: tool})
	}
	flush()
	h.FilesTouched = files.list
	return h, nil
}

// textSectionPrefixes are the role prefixes CursorAdapter.FilterText keeps.
var textSectionPrefixes = []string{"user:", "assistant:", "tool:"}

// textTranscriptExchanges reads the role sections of a filtered text
// transcript: a "user:" section starts an exchange, an "assistant:" section is
// agent text, and a "tool:" section is tool output. Continuation lines belong
// to the section above them.
func textTranscriptExchanges(texts []TextTranscript, opts HandoffOptions) ([]HandoffExchange, string) {
	exchanges := []HandoffExchange{}
	current := &HandoffExchange{}
	leftOff := ""
	role, body := "", []string{}
	flushSection := func() {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		switch role {
		case "user:":
			if current.Prompt != "" || len(current.Steps) > 0 {
				exchanges = append(exchanges, *current)
			}
			current = &HandoffExchange{Prompt: cleanPrompt(text)}
		case "assistant:":
			if text != "" {
				current.Steps = append(current.Steps, HandoffStep{Kind: "text", Text: text})
				leftOff = text
			}
		case "tool:":
			if text != "" {
				current.Steps = append(current.Steps, HandoffStep{Kind: "tool", Tool: &HandoffToolCall{
					Name: "tool", Summary: firstLine(text, handoffSummaryCap),
					Result: trimResult(text, opts.resultLines(), opts.resultBytes()), ResultLines: lineCount(text), ResultBytes: len(text),
				}})
			}
		}
		role, body = "", body[:0]
	}
	for _, transcript := range texts {
		for _, line := range strings.Split(transcript.Content, "\n") {
			lower := strings.ToLower(strings.TrimSpace(line))
			started := false
			for _, prefix := range textSectionPrefixes {
				if strings.HasPrefix(lower, prefix) {
					flushSection()
					role, started = prefix, true
					body = append(body, strings.TrimSpace(strings.TrimSpace(line)[len(prefix):]))
					break
				}
			}
			if !started && role != "" {
				body = append(body, line)
			}
		}
		flushSection()
	}
	if current.Prompt != "" || len(current.Steps) > 0 {
		exchanges = append(exchanges, *current)
	}
	return exchanges, leftOff
}

// handoffEvent is one turn or one tool call, in record order.
type handoffEvent struct {
	record int
	turn   *NormalizedTurn
	call   *NormalizedToolCall
}

// handoffEvents merges turns and tool calls by record index. A tool call
// shares a record with the assistant text that introduced it (Claude) or has
// a record of its own (Codex); within one record the text comes first.
func handoffEvents(view NormalizedView) []handoffEvent {
	events := make([]handoffEvent, 0, len(view.Turns)+len(view.ToolCalls))
	for i := range view.Turns {
		events = append(events, handoffEvent{record: view.Turns[i].RecordIndex, turn: &view.Turns[i]})
	}
	for i := range view.ToolCalls {
		events = append(events, handoffEvent{record: view.ToolCalls[i].RecordIndex, call: &view.ToolCalls[i]})
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].record != events[j].record {
			return events[i].record < events[j].record
		}
		return events[i].turn != nil && events[j].turn == nil
	})
	return events
}

// recordedWorkspace returns the directory the session started in, reduced to
// its base name, and the last git branch the transcript recorded.
func recordedWorkspace(bundle SourceBundle) HandoffWorkspace {
	var out HandoffWorkspace
	if root := workspaceRoot(bundle); root != "" {
		out.Directory = path.Base(root)
	}
	for i := len(bundle.NativeRecords) - 1; i >= 0; i-- {
		if branch := firstStringDeep(bundle.NativeRecords[i], "gitBranch"); branch != "" {
			out.Branch = branch
			break
		}
	}
	return out
}

// workspaceRoot is the first working directory the transcript recorded: where
// the session started, normally the project root. Claude Code stamps every
// record with the shell's current directory, which follows a `cd`, so a later
// cwd can be a subdirectory that would make every relative path wrong.
func workspaceRoot(bundle SourceBundle) string {
	for _, record := range bundle.NativeRecords {
		if cwd := firstStringDeep(record, "cwd"); cwd != "" {
			return path.Clean(cwd)
		}
	}
	return ""
}

func countGaps(gaps []CaptureGap) []HandoffGap {
	counts := map[string]int{}
	for _, gap := range gaps {
		counts[gap.Code]++
	}
	out := make([]HandoffGap, 0, len(counts))
	for code, count := range counts {
		out = append(out, HandoffGap{Code: code, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// cursorTimestamp matches the <timestamp> line Cursor prepends to a prompt.
var cursorTimestamp = regexp.MustCompile(`(?s)^\s*<timestamp>.*?</timestamp>\s*`)

// slashCommandName and slashCommandArgs read the tags Claude Code writes for a
// typed slash command: <command-name>/review-pr</command-name>,
// <command-message>…</command-message>, <command-args>12</command-args>.
var (
	slashCommandName = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	slashCommandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// cleanPrompt shows a prompt as the person typed it. It removes the wrapper
// Cursor puts around a query (<timestamp>…</timestamp> then
// <user_query>…</user_query>), and turns Claude Code's slash-command tags
// back into the command line (/review-pr 12). Other prompts are only trimmed.
func cleanPrompt(text string) string {
	text = cursorTimestamp.ReplaceAllString(strings.TrimSpace(text), "")
	if strings.HasPrefix(text, "<user_query>") && strings.HasSuffix(text, "</user_query>") {
		text = stripHarnessTag(text, "user_query")
	}
	if strings.HasPrefix(strings.TrimSpace(text), "<command-") {
		if name := slashCommandName.FindStringSubmatch(text); name != nil {
			command := strings.TrimSpace(name[1])
			if args := slashCommandArgs.FindStringSubmatch(text); args != nil && strings.TrimSpace(args[1]) != "" {
				command += " " + strings.TrimSpace(args[1])
			}
			return command
		}
	}
	return strings.TrimSpace(text)
}

// stripHarnessTag removes a Claude Code harness wrapper such as
// <bash-input>…</bash-input> around a record's text.
func stripHarnessTag(text, tag string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "<"+tag+">")
	return strings.TrimSuffix(text, "</"+tag+">")
}

func lineCount(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
}

// trimResult keeps the head and tail of a tool result: at most maxLines
// lines, then at most maxBytes bytes, each split evenly, with a marker where
// content was removed.
func trimResult(text string, maxLines, maxBytes int) string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		head, tail := (maxLines+1)/2, maxLines/2
		omitted := len(lines) - head - tail
		kept := append(append([]string{}, lines[:head]...), fmt.Sprintf("… %d lines omitted …", omitted))
		lines = append(kept, lines[len(lines)-tail:]...)
		text = strings.Join(lines, "\n")
	}
	if len(text) > maxBytes {
		half := maxBytes / 2
		head, tail := TruncateUTF8(text, half), text[len(text)-half:]
		for len(tail) > 0 && !isRuneStart(tail[0]) {
			tail = tail[1:]
		}
		text = head + fmt.Sprintf("\n… %d bytes omitted …\n", len(text)-len(head)-len(tail)) + tail
	}
	return text
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// TruncateUTF8 returns at most n bytes of s without splitting a character:
// a cut that would land inside a UTF-8 sequence moves back to its start.
func TruncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// firstLine returns the first non-empty line of s, capped at max bytes.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	if len(s) > max {
		s = TruncateUTF8(s, max) + "…"
	}
	return s
}

var (
	shellToolNames  = map[string]bool{"bash": true, "shell": true, "exec_command": true, "local_shell_call": true, "commandexecution": true, "run_terminal_cmd": true}
	readToolNames   = map[string]bool{"read": true, "read_file": true, "view": true}
	editToolNames   = map[string]bool{"edit": true, "multiedit": true, "write": true, "notebookedit": true, "apply_patch": true, "str_replace": true, "strreplace": true, "search_replace": true, "edit_file": true, "create_file": true, "write_file": true, "delete_file": true}
	searchToolNames = map[string]bool{"grep": true, "glob": true, "search": true, "codebase_search": true, "grep_search": true, "file_search": true}
	agentToolNames  = map[string]bool{"agent": true, "task": true}
	planToolNames   = map[string]bool{"todowrite": true, "todo_write": true, "update_plan": true}
)

// toolSummary renders one line describing a call from its retained
// arguments. Edit bodies are deliberately never included. Paths under root
// are shown relative to it.
func toolSummary(name string, input map[string]any, raw map[string]any, root string) string {
	lower := strings.ToLower(name)
	switch {
	case shellToolNames[lower]:
		if command := argumentText(input, "command", "cmd"); command != "" {
			return firstLine(command, handoffSummaryCap)
		}
		if command := argumentText(raw, "command"); command != "" {
			return firstLine(command, handoffSummaryCap)
		}
	case readToolNames[lower]:
		file := relativeTo(firstString(input, "file_path", "path", "target_file"), root)
		if file == "" {
			break
		}
		if offset, ok := input["offset"].(float64); ok {
			if limit, ok := input["limit"].(float64); ok {
				return fmt.Sprintf("%s (lines %d–%d)", file, int(offset), int(offset)+int(limit)-1)
			}
			return fmt.Sprintf("%s (from line %d)", file, int(offset))
		}
		return file
	case editToolNames[lower]:
		if files := touchedFiles(name, input, raw); len(files) > 0 {
			for i := range files {
				files[i] = relativeTo(files[i], root)
			}
			return strings.Join(files, ", ")
		}
		return ""
	case searchToolNames[lower]:
		pattern := firstString(input, "pattern", "query", "glob_pattern", "glob")
		where := relativeTo(firstString(input, "path", "target_directory"), root)
		if pattern != "" && where != "" {
			return firstLine(pattern+" in "+where, handoffSummaryCap)
		}
		if pattern != "" {
			return firstLine(pattern, handoffSummaryCap)
		}
	case agentToolNames[lower]:
		if description := firstString(input, "description"); description != "" {
			return firstLine(description, handoffSummaryCap)
		}
		if prompt := firstString(input, "prompt"); prompt != "" {
			return firstLine(prompt, handoffAgentPromptCap)
		}
	case planToolNames[lower]:
		return "updated the plan"
	}
	if title := firstString(input, "title"); title != "" {
		return firstLine(title, handoffSummaryCap)
	}
	if input != nil {
		encoded, err := json.Marshal(input)
		if err == nil && string(encoded) != "{}" {
			return firstLine(string(encoded), handoffSummaryCap)
		}
	}
	// A custom tool (Codex exec, apply_patch) carries its input as one raw
	// string. Codex's exec tool is JavaScript that usually calls
	// tools.exec_command({cmd: …}); the command is the useful part.
	if text, ok := raw["input"].(string); ok {
		if command := codexExecCommand(text); command != "" {
			return firstLine(command, handoffSummaryCap)
		}
		return firstLine(text, handoffSummaryCap)
	}
	return ""
}

// codexExecCommand returns the cmd of the first exec_command({…}) call in a
// Codex exec script, or "" when there is none or it does not decode.
func codexExecCommand(script string) string {
	at := strings.Index(script, "exec_command(")
	if at < 0 {
		return ""
	}
	var args map[string]any
	if err := json.NewDecoder(strings.NewReader(script[at+len("exec_command("):])).Decode(&args); err != nil {
		return ""
	}
	return argumentText(args, "cmd", "command")
}

// argumentText returns the first named argument as text, joining a list of
// strings (Codex shell's argv) with spaces.
func argumentText(args map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := args[key].(type) {
		case string:
			if value != "" {
				return value
			}
		case []any:
			parts := make([]string, 0, len(value))
			for _, part := range value {
				if s, ok := part.(string); ok {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, " ")
			}
		}
	}
	return ""
}

// touchedFiles lists the files an editing call names: its path argument, or
// for apply_patch the files named in the patch headers.
func touchedFiles(name string, input map[string]any, raw map[string]any) []string {
	lower := strings.ToLower(name)
	if !editToolNames[lower] {
		return nil
	}
	if file := firstString(input, "file_path", "path", "target_file", "notebook_path"); file != "" {
		return []string{file}
	}
	patch := firstString(input, "input", "patch")
	if patch == "" {
		patch, _ = raw["input"].(string)
	}
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: ", "*** Move to: "} {
			if strings.HasPrefix(line, prefix) {
				out = append(out, strings.TrimSpace(strings.TrimPrefix(line, prefix)))
			}
		}
	}
	return out
}

// relativeTo shows a file under root relative to it, so the list reads the
// same on a machine where the repository lives elsewhere.
func relativeTo(file, root string) string {
	if root == "" || !path.IsAbs(file) {
		return file
	}
	clean := path.Clean(file)
	if strings.HasPrefix(clean, root+"/") {
		return strings.TrimPrefix(clean, root+"/")
	}
	return clean
}

type fileSet struct {
	seen map[string]bool
	list []string
}

func (s *fileSet) add(file string) {
	if file == "" {
		return
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if !s.seen[file] {
		s.seen[file] = true
		s.list = append(s.list, file)
	}
}

// planItems reads a plan-writing call: Claude's TodoWrite {todos: [{content,
// status}]}, Codex's update_plan {plan: [{step, status}]}, and Cursor's
// todo_write. It returns nil for any other call, and for a plan call whose
// item list it cannot find.
func planItems(name string, input map[string]any) []HandoffPlanItem {
	if !planToolNames[strings.ToLower(name)] {
		return nil
	}
	var list []any
	found := false
	for _, key := range []string{"todos", "plan", "items"} {
		if value, ok := input[key].([]any); ok {
			list, found = value, true
			break
		}
	}
	if !found {
		// Arguments that did not decode, or a shape this reader does not
		// know, say nothing about the plan; they must not erase an earlier
		// one. An explicit empty list does clear it.
		return nil
	}
	items := []HandoffPlanItem{}
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text := firstString(entry, "content", "step", "description", "title", "text")
		if text == "" {
			continue
		}
		items = append(items, HandoffPlanItem{Text: text, Status: firstString(entry, "status")})
	}
	return items
}

// FitHandoff returns a copy of h trimmed until measure(copy) is at most
// maxBytes, applying the budget steps in order and recording each in
// Elisions. Each step is applied to the oldest exchanges first, and to no more
// of them than needed. The protected tail — steps in the last
// handoffKeptExchanges exchanges, but no more than the last handoffKeptSteps
// steps, so one long autonomous exchange can still be trimmed — is never
// touched by the first three steps. Prompts are only ever truncated, and
// LeftOff is never trimmed. fits is false when the result is still over
// budget after every step. maxBytes <= 0 means no budget.
//
// Each step's effect only grows with the number of exchanges it is applied
// to, so the smallest sufficient prefix is found by binary search: a handful
// of measurements per step rather than one per exchange, which matters when
// measure renders a multi-megabyte session.
func FitHandoff(h Handoff, maxBytes int, measure func(Handoff) int) (Handoff, bool) {
	out := cloneHandoff(h)
	if maxBytes <= 0 || measure(out) <= maxBytes {
		return out, true
	}
	steps := []struct {
		kind  string
		apply func(steps []HandoffStep, limit int) ([]HandoffStep, int)
	}{
		{HandoffElisionToolOutput, dropToolOutput},
		{HandoffElisionToolCalls, collapseToolCalls},
		{HandoffElisionAssistantText, shortenAssistantText},
	}
	for _, step := range steps {
		// Protection is computed once, before the step: a step only changes
		// steps outside the protected tail, so the tail it computes stays
		// the same while the step runs.
		limits := protectedStart(out.Exchanges, handoffKeptExchanges, handoffKeptSteps)
		apply := func(k int) (Handoff, HandoffElision) {
			trial := cloneHandoff(out)
			elision := HandoffElision{Kind: step.kind}
			for i := 0; i < k; i++ {
				var n int
				trial.Exchanges[i].Steps, n = step.apply(trial.Exchanges[i].Steps, limits[i])
				if n > 0 {
					if elision.First == 0 {
						elision.First = i + 1
					}
					elision.Last, elision.Count = i+1, elision.Count+n
				}
			}
			return trial, elision
		}
		trial, elision, fits := smallestFittingPrefix(len(out.Exchanges), maxBytes, measure, apply)
		out = trial
		if elision.Count > 0 {
			out.Elisions = append(out.Elisions, elision)
		}
		if fits {
			return out, true
		}
	}
	// Prompts are truncated, never dropped, and every exchange is eligible.
	apply := func(k int) (Handoff, HandoffElision) {
		trial := cloneHandoff(out)
		elision := HandoffElision{Kind: HandoffElisionPromptText}
		for i := 0; i < k; i++ {
			exchange := &trial.Exchanges[i]
			if len(exchange.Prompt) > handoffPromptCap {
				exchange.Prompt = TruncateUTF8(exchange.Prompt, handoffPromptCap)
				exchange.PromptTruncated = true
				if elision.First == 0 {
					elision.First = i + 1
				}
				elision.Last, elision.Count = i+1, elision.Count+1
			}
		}
		return trial, elision
	}
	trial, elision, fits := smallestFittingPrefix(len(out.Exchanges), maxBytes, measure, apply)
	out = trial
	if elision.Count > 0 {
		out.Elisions = append(out.Elisions, elision)
	}
	return out, fits
}

// smallestFittingPrefix applies a step to the first k exchanges for the
// smallest k whose result fits, or to all n when none does. apply(k) must not
// modify its input and its result size must not grow with k. The returned
// handoff does not yet carry the step's elision.
func smallestFittingPrefix(n, maxBytes int, measure func(Handoff) int, apply func(k int) (Handoff, HandoffElision)) (Handoff, HandoffElision, bool) {
	all, allElision := apply(n)
	if measure(withElision(all, allElision)) > maxBytes {
		return all, allElision, false
	}
	lo, hi := 1, n // the answer is in [lo, hi]; apply(hi) fits
	best, bestElision := all, allElision
	for lo < hi {
		mid := (lo + hi) / 2
		trial, elision := apply(mid)
		if measure(withElision(trial, elision)) <= maxBytes {
			hi, best, bestElision = mid, trial, elision
		} else {
			lo = mid + 1
		}
	}
	return best, bestElision, true
}

// withElision is h as it will render once elision is recorded, so a
// measurement includes the footer line that describes it.
func withElision(h Handoff, elision HandoffElision) Handoff {
	if elision.Count == 0 {
		return h
	}
	h.Elisions = append(append([]HandoffElision(nil), h.Elisions...), elision)
	return h
}

// protectedStart returns, for each exchange, the index of its first
// protected step: steps from there on belong to the protected tail. A step is
// protected when its exchange is among the last keptExchanges and it is among
// the last keptSteps steps of the whole conversation.
func protectedStart(exchanges []HandoffExchange, keptExchanges, keptSteps int) []int {
	starts := make([]int, len(exchanges))
	remaining := keptSteps
	for i := len(exchanges) - 1; i >= 0; i-- {
		n := len(exchanges[i].Steps)
		switch {
		case i < len(exchanges)-keptExchanges || remaining == 0:
			starts[i] = n
		case n <= remaining:
			starts[i] = 0
			remaining -= n
		default:
			starts[i] = n - remaining
			remaining = 0
		}
	}
	return starts
}

func dropToolOutput(steps []HandoffStep, limit int) ([]HandoffStep, int) {
	n := 0
	for _, step := range steps[:limit] {
		if step.Tool != nil && step.Tool.Result != "" {
			step.Tool.Result, step.Tool.ResultOmitted = "", true
			n++
		}
	}
	return steps, n
}

// collapseToolCalls replaces the tool-call steps before limit with one
// "collapsed" step, at the position of the first, that counts them by name.
func collapseToolCalls(steps []HandoffStep, limit int) ([]HandoffStep, int) {
	counts := map[string]int{}
	var order []string
	out := make([]HandoffStep, 0, len(steps))
	at, total := -1, 0
	for i, step := range steps {
		if i >= limit || step.Tool == nil {
			out = append(out, step)
			continue
		}
		if at < 0 {
			at = len(out)
			out = append(out, HandoffStep{Kind: "collapsed"})
		}
		if counts[step.Tool.Name] == 0 {
			order = append(order, step.Tool.Name)
		}
		counts[step.Tool.Name]++
		total++
	}
	if total == 0 {
		return steps, 0
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%s ×%d", name, counts[name]))
	}
	noun := "tool calls"
	if total == 1 {
		noun = "tool call"
	}
	out[at].Text = fmt.Sprintf("%d %s: %s", total, noun, strings.Join(parts, ", "))
	return out, total
}

func shortenAssistantText(steps []HandoffStep, limit int) ([]HandoffStep, int) {
	n := 0
	for i := range steps[:limit] {
		step := &steps[i]
		if (step.Kind == "text" || step.Kind == "summary") && len(step.Text) > handoffAssistantTextCap {
			step.Text = TruncateUTF8(step.Text, handoffAssistantTextCap)
			step.TextTruncated = true
			n++
		}
	}
	return steps, n
}

// cloneHandoff copies everything FitHandoff mutates.
func cloneHandoff(h Handoff) Handoff {
	out := h
	out.Elisions = append([]HandoffElision(nil), h.Elisions...)
	out.Exchanges = make([]HandoffExchange, len(h.Exchanges))
	for i, exchange := range h.Exchanges {
		copied := exchange
		copied.Steps = make([]HandoffStep, len(exchange.Steps))
		for j, step := range exchange.Steps {
			if step.Tool != nil {
				tool := *step.Tool
				step.Tool = &tool
			}
			copied.Steps[j] = step
		}
		out.Exchanges[i] = copied
	}
	return out
}

// HandoffRenderOptions controls markdown rendering.
type HandoffRenderOptions struct {
	// Preamble includes the note addressed to the receiving agent.
	Preamble bool
}

var harnessDisplayNames = map[string]string{"claude": "Claude Code", "codex": "Codex", "cursor": "Cursor"}

func harnessDisplayName(name string) string {
	if display := harnessDisplayNames[name]; display != "" {
		return display
	}
	if name == "" {
		return "coding agent"
	}
	return name
}

// RenderHandoffMarkdown renders h as a prompt another coding agent can
// continue from. Summary sections come before the conversation so an agent
// that reads only the top still has the essentials.
func RenderHandoffMarkdown(h Handoff, opts HandoffRenderOptions) []byte {
	var b strings.Builder
	source := h.Session.Source
	if source == "" {
		source = "unknown"
	}
	id := h.Session.ArchiveSessionID
	if id == "" {
		id = h.Session.NativeSessionID
	}
	fmt.Fprintf(&b, "<!-- agent-archive handoff v%d · %s · session %s · source: %s -->\n", HandoffVersion, h.Session.Harness, id, source)
	fmt.Fprintf(&b, "# Handoff: continuing a %s session\n\n", harnessDisplayName(h.Session.Harness))
	if opts.Preamble {
		b.WriteString("> You are picking up work another coding agent started. The conversation\n" +
			"> below is a filtered record: injected instructions and credentials were\n" +
			"> removed (a `[REDACTED]` marker is not a real value), tool output is\n" +
			"> trimmed, and edit bodies are omitted. Before acting, check the\n" +
			"> repository's current state (`git status`, the files listed below) rather\n" +
			"> than trusting the record. Ask the person if the next step is unclear.\n\n")
	}

	b.WriteString("## Session\n")
	agent := harnessDisplayName(h.Session.Harness)
	if h.Session.HarnessVersion != "" {
		agent += " " + h.Session.HarnessVersion
	}
	if len(h.Session.Models) > 0 {
		agent += " · models: " + strings.Join(h.Session.Models, ", ")
	}
	fmt.Fprintf(&b, "- Agent: %s\n", agent)
	var when []string
	if h.Session.StartedAt != nil {
		when = append(when, "started "+h.Session.StartedAt.Format("2006-01-02 15:04 UTC"))
	}
	if h.Session.LastActivityAt != nil {
		when = append(when, "last activity "+h.Session.LastActivityAt.Format("2006-01-02 15:04 UTC"))
	}
	if h.Session.State != "" && h.Session.State != MetadataStateUnknown {
		when = append(when, "state: "+string(h.Session.State))
	}
	if len(when) > 0 {
		fmt.Fprintf(&b, "- %s\n", capitalize(strings.Join(when, " · ")))
	}
	var where []string
	if h.Workspace.Branch != "" {
		where = append(where, "branch: "+h.Workspace.Branch)
	}
	if h.Workspace.Directory != "" {
		where = append(where, "directory: "+h.Workspace.Directory)
	}
	if len(where) > 0 {
		fmt.Fprintf(&b, "- %s (as recorded)\n", capitalize(strings.Join(where, " · ")))
	}
	if h.ToolResultsUnavailable {
		fmt.Fprintf(&b, "- %s does not record tool results, so none appear below.\n", agent)
	}
	b.WriteString("\n")

	if h.LeftOff != "" {
		fmt.Fprintf(&b, "## Where it left off\n%s\n\n", h.LeftOff)
	}
	if len(h.Plan) > 0 {
		b.WriteString("## Plan\n")
		for _, item := range h.Plan {
			box, suffix := "[ ]", ""
			switch strings.ToLower(item.Status) {
			case "completed", "done":
				box = "[x]"
			case "in_progress", "in-progress", "active":
				suffix = " (in progress)"
			}
			fmt.Fprintf(&b, "- %s %s%s\n", box, item.Text, suffix)
		}
		b.WriteString("\n")
	}
	if len(h.FilesTouched) > 0 {
		fmt.Fprintf(&b, "## Files touched\n%s\n\n", strings.Join(h.FilesTouched, ", "))
	}

	b.WriteString("## Conversation\n")
	for i, exchange := range h.Exchanges {
		if exchange.Prompt == "" {
			fmt.Fprintf(&b, "\n### %d · Before the first prompt\n", i+1)
		} else {
			fmt.Fprintf(&b, "\n### %d · Person\n", i+1)
			prompt := exchange.Prompt
			if exchange.PromptTruncated {
				prompt += " …(truncated)"
			}
			b.WriteString(quote(prompt))
		}
		inTools := false
		for _, step := range exchange.Steps {
			switch step.Kind {
			case "text":
				text := step.Text
				if step.TextTruncated {
					text += " …(shortened)"
				}
				fmt.Fprintf(&b, "\n**Agent:** %s\n", text)
				inTools = false
			case "shell":
				fmt.Fprintf(&b, "\n**Person ran:** `%s`\n", firstLine(step.Text, handoffSummaryCap))
				inTools = false
			case "summary":
				text := step.Text
				if step.TextTruncated {
					text += " …(shortened)"
				}
				fmt.Fprintf(&b, "\n**Conversation compacted.** The agent continued from this summary:\n\n%s", quote(text))
				inTools = false
			case "tool":
				if !inTools {
					b.WriteString("\n")
					inTools = true
				}
				renderTool(&b, step.Tool)
			case "collapsed":
				if !inTools {
					b.WriteString("\n")
					inTools = true
				}
				fmt.Fprintf(&b, "- %s\n", step.Text)
			}
		}
	}

	var footer []string
	if len(h.Elisions) > 0 {
		parts := make([]string, 0, len(h.Elisions))
		for _, e := range h.Elisions {
			parts = append(parts, describeElision(e))
		}
		footer = append(footer, "Omitted to fit the size limit: "+strings.Join(parts, "; ")+".")
		if h.FullRecordPath != "" {
			footer = append(footer, fmt.Sprintf("Full record: %s (read it for anything omitted here).", h.FullRecordPath))
		}
	}
	if len(h.Gaps) > 0 {
		parts := make([]string, 0, len(h.Gaps))
		for _, gap := range h.Gaps {
			parts = append(parts, fmt.Sprintf("%s ×%d", gap.Code, gap.Count))
		}
		footer = append(footer, "Capture gaps: "+strings.Join(parts, ", ")+".")
	}
	if len(footer) > 0 {
		b.WriteString("\n---\n")
		b.WriteString(strings.Join(footer, "\n"))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func renderTool(b *strings.Builder, tool *HandoffToolCall) {
	line := "- `" + tool.Name + "`"
	if tool.Summary != "" {
		line += " " + tool.Summary
	}
	switch {
	case tool.IsError:
		line += " → error"
	case tool.ResultOmitted:
		line += fmt.Sprintf(" → %d lines (output omitted)", tool.ResultLines)
	}
	b.WriteString(line + "\n")
	if tool.Result != "" {
		fence := codeFence(tool.Result)
		fmt.Fprintf(b, "  %s\n", fence)
		for _, resultLine := range strings.Split(tool.Result, "\n") {
			fmt.Fprintf(b, "  %s\n", resultLine)
		}
		fmt.Fprintf(b, "  %s\n", fence)
	}
}

// codeFence returns a backtick fence longer than any backtick run in text.
func codeFence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	if longest < 3 {
		return "```"
	}
	return strings.Repeat("`", longest+1)
}

func quote(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			b.WriteString(">\n")
			continue
		}
		b.WriteString("> " + line + "\n")
	}
	return b.String()
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func describeElision(e HandoffElision) string {
	span := fmt.Sprintf("exchange %d", e.First)
	if e.Last != e.First {
		span = fmt.Sprintf("exchanges %d–%d", e.First, e.Last)
	}
	switch e.Kind {
	case HandoffElisionToolOutput:
		return fmt.Sprintf("output of %d tool calls in %s", e.Count, span)
	case HandoffElisionToolCalls:
		return fmt.Sprintf("%d tool calls in %s collapsed to counts", e.Count, span)
	case HandoffElisionAssistantText:
		return fmt.Sprintf("%d agent messages in %s shortened", e.Count, span)
	case HandoffElisionPromptText:
		return fmt.Sprintf("%d long prompts in %s truncated", e.Count, span)
	}
	return fmt.Sprintf("%d %s in %s", e.Count, e.Kind, span)
}
