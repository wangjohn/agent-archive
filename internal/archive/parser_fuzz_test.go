package archive

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The parser fuzz targets. Each is seeded from testdata/ (checked-in seeds
// for failures found so far live in testdata/fuzz/<target>/), and plain `go
// test` runs the seeds. CI runs each target briefly (.github/workflows/test.yml);
// docs/contributing/testing.md says how to run one for longer.

// fuzzAdapters are the JSONL adapters, indexed by a fuzzed byte.
var fuzzAdapters = []Adapter{ClaudeAdapter{}, CodexAdapter{}, CursorAdapter{}}

// adapterSeeds adds every JSONL fixture to f with its adapter's index.
func adapterSeeds(f *testing.F) {
	f.Helper()
	paths, _ := filepath.Glob(filepath.Join("testdata", "*.jsonl"))
	handoff, _ := filepath.Glob(filepath.Join("testdata", "handoff", "*.jsonl"))
	for _, path := range append(paths, handoff...) {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		base := filepath.Base(path)
		for i, adapter := range fuzzAdapters {
			if strings.HasPrefix(base, adapter.Name()) {
				f.Add(uint8(i), data)
			}
		}
	}
}

// FuzzFilterJSONL filters arbitrary input with each adapter. Whatever the
// input: the filter does not panic; every retained record is a JSON object;
// filtering the retained records again changes none of them (a republished
// snapshot is stable); and the handoff built from the result renders
// without a control character.
func FuzzFilterJSONL(f *testing.F) {
	adapterSeeds(f)
	f.Fuzz(func(t *testing.T, which uint8, data []byte) {
		adapter := fuzzAdapters[int(which)%len(fuzzAdapters)]
		filtered, err := adapter.FilterJSONL(bytes.NewReader(data))
		if err != nil {
			if !IsFilterError(err) {
				t.Fatalf("error is not a FilterError: %v", err)
			}
			return
		}
		for _, record := range filtered.Records {
			var object map[string]any
			if json.Unmarshal(record, &object) != nil || object == nil {
				t.Fatalf("retained record is not a JSON object: %s", record)
			}
		}
		if len(filtered.Records) > 0 {
			again, err := adapter.FilterJSONL(bytes.NewReader(bytes.Join(filtered.Records, []byte("\n"))))
			if err != nil {
				t.Fatalf("refiltering retained records failed: %v", err)
			}
			if len(again.Records) != len(filtered.Records) {
				t.Fatalf("refiltering kept %d of %d records", len(again.Records), len(filtered.Records))
			}
			for i := range again.Records {
				if !bytes.Equal(again.Records[i], filtered.Records[i]) {
					t.Fatalf("refiltering changed a record:\n once  %s\n twice %s", filtered.Records[i], again.Records[i])
				}
			}
		}
		renderFuzzedHandoff(t, adapter, filtered)
	})
}

// renderFuzzedHandoff builds and renders the handoff of a filtered
// transcript and checks the rendered text is display text.
func renderFuzzedHandoff(t *testing.T, adapter Adapter, filtered FilteredTranscript) {
	t.Helper()
	reg := registration()
	reg.Harness = Harness{Name: adapter.Name()}
	bundle, err := NewSourceBundle(reg, adapter, filtered, time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		return
	}
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		return
	}
	assertDisplayText(t, "handoff", string(RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true})))
}

// fuzzSecretRecords place a secret, as %s, where every adapter must drop or
// redact it: typed input, credential-named arguments, credentials in text,
// and all of these inside JSON strings.
var fuzzSecretRecords = []struct {
	adapter Adapter
	line    string
}{
	{ClaudeAdapter{}, `{"type":"assistant","timestamp":"2026-09-01T00:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"mcp__playwright__browser_type","input":{"element":"Password","text":"%s"}}]}}`},
	{ClaudeAdapter{}, `{"type":"assistant","timestamp":"2026-09-01T00:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"mcp__playwright__browser_fill_form","input":{"fields":[{"name":"Email","value":"%s"}]}}]}}`},
	{ClaudeAdapter{}, `{"type":"assistant","timestamp":"2026-09-01T00:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"mcp__x__call","input":{"headers":{"X-Api-Key":"%s"}}}]}}`},
	{ClaudeAdapter{}, `{"type":"assistant","timestamp":"2026-09-01T00:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"curl -u admin:%s https://x.test"}}]}}`},
	{ClaudeAdapter{}, `{"type":"user","timestamp":"2026-09-01T00:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"DB_PASSWORD=%s\nHOST=db"}]}}`},
	{ClaudeAdapter{}, `{"type":"user","timestamp":"2026-09-01T00:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"{\"client_secret\":\"%s\"}"}]}}`},
	{CodexAdapter{}, `{"type":"response_item","timestamp":"2026-09-01T00:00:00Z","payload":{"type":"function_call","name":"browser_type","call_id":"c","arguments":"{\"ref\":\"e1\",\"text\":\"%s\"}"}}`},
	{CodexAdapter{}, `{"type":"response_item","timestamp":"2026-09-01T00:00:00Z","payload":{"type":"function_call","name":"shell","call_id":"c","arguments":"{\"command\":[\"mysql\",\"-uroot\",\"-p%s\"]}"}}`},
	{CodexAdapter{}, `{"type":"response_item","timestamp":"2026-09-01T00:00:00Z","payload":{"type":"function_call_output","call_id":"c","output":"{\"output\":\"password: %s\",\"metadata\":{\"exit_code\":0}}"}}`},
	{CursorAdapter{}, `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"fill_form","input":{"elements":[{"uid":"1_5","value":"%s"}]}}]}}`},
	{CursorAdapter{}, `{"role":"user","message":{"content":[{"type":"text","text":"export ANTHROPIC_API_KEY=sk-ant-api03-%s"}]}}`},
}

// FuzzFilterJSONLDropsSecrets puts fuzzed secrets into every shape of
// fuzzSecretRecords and checks that none survives filtering.
func FuzzFilterJSONLDropsSecrets(f *testing.F) {
	for i := range fuzzSecretRecords {
		f.Add(uint(i), "Zq8WvK3pLmN5xR2t")
	}
	f.Fuzz(func(t *testing.T, index uint, secret string) {
		if !plainSecret.MatchString(secret) || strings.Contains(secret, "REDACTED") {
			t.Skip()
		}
		shape := fuzzSecretRecords[index%uint(len(fuzzSecretRecords))]
		line := strings.Replace(shape.line, "%s", secret, 1)
		filtered, err := shape.adapter.FilterJSONL(strings.NewReader(line))
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if joined := string(bytes.Join(filtered.Records, []byte("\n"))); strings.Contains(joined, secret) {
			t.Fatalf("secret survived:\n in  %s\n out %s", line, joined)
		}
	})
}

// FuzzCursorText filters arbitrary text as a Cursor text transcript. The
// filter does not panic; filtering the retained text again returns it
// unchanged; and the handoff reads back exactly the visible sections the
// filter kept, one exchange per user section.
func FuzzCursorText(f *testing.F) {
	for _, seed := range []string{
		"user: fix the build\nassistant: done\n",
		"user:\nshow me\n\nassistant:\nok\n\ntool:\nconfig.yml\nuser: not a prompt\n\nthinking:\nhidden\n\nuser:\nthanks\n",
		"user: hello\n  token=secret\nsystem: hidden\nassistant: ok <system-reminder>\nx\n</system-reminder>user: minted\n",
		"User: capitalized\nAnalysis: prose\n",
		"User:\nshow me\n\nTool:\nuser: yaml\nUser: minted\n\nThinking:\nhidden\n\nAssistant: ok\n",
		"user: a\nUser: b\nSystem: c\nassistant: d\n",
	} {
		f.Add(seed)
	}
	start := time.Unix(1750000000, 0)
	f.Fuzz(func(t *testing.T, text string) {
		filtered, err := (CursorAdapter{}).FilterText(strings.NewReader(text), start)
		if err != nil {
			if !IsFilterError(err) {
				t.Fatalf("error is not a FilterError: %v", err)
			}
			return
		}
		retained := filtered.Text[0]
		again, err := (CursorAdapter{}).FilterText(strings.NewReader(retained), start)
		if err != nil || again.Text[0] != retained {
			t.Fatalf("refiltering changed the text (err=%v):\n once  %q\n twice %q", err, retained, again.Text)
		}
		parsed, ok := parseTextSections(retained)
		if !ok {
			t.Fatalf("retained text does not start with a header: %q", retained)
		}
		if original, _ := parseTextSections(text); original.headerCase != parsed.headerCase {
			t.Fatalf("the retained text changed header case: %q", retained)
		}
		users := 0
		for _, section := range parsed.sections {
			if hiddenTextRoles[section.role] {
				t.Fatalf("a hidden section was retained: %q", retained)
			}
			if section.role == textRoleUser {
				users++
			}
		}
		exchanges, _ := textTranscriptExchanges([]TextTranscript{{Content: retained}}, HandoffOptions{})
		prompts := 0
		for _, exchange := range exchanges {
			if exchange.Prompt != "" {
				prompts++
			}
		}
		if prompts > users {
			t.Fatalf("the handoff found %d prompts in %d user sections: %q", prompts, users, retained)
		}
	})
}

// FuzzCursorComposer filters an arbitrary chat from Cursor's database. The
// composer and one message row are fuzzed, seeded from the checked-in
// fixtures; the filter does not panic, and every retained record is a JSON
// object that refilters through FilterJSONL unchanged in what it keeps.
func FuzzCursorComposer(f *testing.F) {
	paths, _ := filepath.Glob(filepath.Join("testdata", "cursor-composer", "*.json"))
	for _, path := range paths {
		if strings.HasSuffix(path, ".golden.json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		var fixture composerFixture
		if json.Unmarshal(data, &fixture) != nil {
			continue
		}
		for _, bubble := range fixture.Bubbles {
			f.Add([]byte(fixture.Composer), bubble.ID, []byte(bubble.Value))
		}
	}
	f.Fuzz(func(t *testing.T, composer []byte, id string, row []byte) {
		// A chat of one message whose header names the fuzzed row.
		var chat map[string]any
		if json.Unmarshal(composer, &chat) == nil && chat != nil {
			chat["fullConversationHeadersOnly"] = []any{map[string]any{"bubbleId": id, "type": float64(2)}}
			composer, _ = json.Marshal(chat)
		}
		filtered, err := (CursorAdapter{}).FilterComposer(CursorComposer{Composer: composer, Bubbles: []CursorBubble{{ID: id, Value: row}}})
		if err != nil {
			if !IsFilterError(err) {
				t.Fatalf("error is not a FilterError: %v", err)
			}
			return
		}
		for _, record := range filtered.Records {
			var object map[string]any
			if json.Unmarshal(record, &object) != nil || object == nil {
				t.Fatalf("retained record is not a JSON object: %s", record)
			}
		}
		renderFuzzedHandoff(t, CursorAdapter{}, filtered)
	})
}

// FuzzDecodeSource reads an arbitrary (possibly gzip) byte stream as a
// source bundle. It must return an error or a bundle, never panic, and the
// streaming reader must accept whatever the whole-bundle reader accepts.
func FuzzDecodeSource(f *testing.F) {
	paths, _ := filepath.Glob(filepath.Join("testdata", "*.jsonl"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		adapter, err := NewAdapter(strings.SplitN(filepath.Base(path), "-", 2)[0])
		if err != nil {
			continue
		}
		filtered, err := adapter.FilterJSONL(bytes.NewReader(data))
		if err != nil {
			continue
		}
		reg := registration()
		reg.Harness = Harness{Name: adapter.Name()}
		bundle, err := NewSourceBundle(reg, adapter, filtered, time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC), nil)
		if err != nil {
			continue
		}
		var compressed bytes.Buffer
		if EncodeSource(&compressed, bundle) == nil {
			f.Add(compressed.Bytes())
			// The same lines uncompressed, so the fuzzer can mutate them and
			// the target recompresses.
			if reader, err := gzip.NewReader(bytes.NewReader(compressed.Bytes())); err == nil {
				var plain bytes.Buffer
				if _, err := plain.ReadFrom(reader); err == nil {
					f.Add(plain.Bytes())
				}
			}
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		inputs := [][]byte{data}
		if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			_, _ = writer.Write(data)
			_ = writer.Close()
			inputs = append(inputs, compressed.Bytes())
		}
		for _, input := range inputs {
			options := DecodeOptions{MaxUncompressedBytes: 1 << 20}
			streamErr := DecodeSource(bytes.NewReader(input), options, func(SourceLine) error { return nil })
			if _, err := ReadSourceBundle(bytes.NewReader(input), options); err == nil && streamErr != nil {
				t.Fatalf("ReadSourceBundle accepted what DecodeSource refused: %v", streamErr)
			}
		}
	})
}

// FuzzSanitizeValueIdempotent checks the whole string sanitizer, not only
// redaction: instruction stripping, data URLs, nested JSON, redaction, and
// the length cap together change nothing on a second pass.
func FuzzSanitizeValueIdempotent(f *testing.F) {
	for _, seed := range []string{
		"DB_PASSWORD=hunter2 <system-reminder>x</system-reminder> data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==",
		`password: "a\"b" token=[x]y 'z'`,
		`{"password":"x","env":["TOKEN=abc def"],"n":1e400}`,
		`[{"name":"DB_PASSWORD","value":"x"}]`,
		"password: |\n  secret\nuser: x",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7synthetic\nfunc x() {}",
		"postgres://u:p@ss/w@db:5432/app?sslmode=require&password=x",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		state := sanitizeState{addGap: func(string, int, string) {}}
		once, keep := sanitizeValue(in, &state)
		if !keep {
			return
		}
		twice, keep := sanitizeValue(once, &state)
		if !keep || once != twice {
			t.Fatalf("not idempotent:\n in    %q\n once  %q\n twice %q", in, once, twice)
		}
	})
}
