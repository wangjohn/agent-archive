package cli

import (
	"bytes"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/state"
	"strings"
	"testing"
	"time"
)

func TestThreadTriageDecoderPanicDiagnostic(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	env := testEnv(t, home, at)
	decoder := diagnosticPanicDecoder{observedHookDecoder{decode: func(agentapi.HookInput) []agentapi.LifecycleEvent { panic("decoder failure") }}}
	env.Agents = registryWithSyntheticHooks(t, syntheticHooks{}, decoder)
	var stderr bytes.Buffer
	payload := `{"opaque":"triage","tree":` + quoteJSON(project) + `,"cwd":` + quoteJSON(project) + `}`
	code := runHookCommand([]string{"--harness", "native-synth"}, strings.NewReader(payload), &stderr, env)
	if code != 0 || !strings.Contains(stderr.String(), "decoder failure") {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
	ds, err := capture.ReadDiagnostics(home)
	if err != nil || len(ds) != 1 || ds[0].Code != capture.DiagnosticHookFailed || ds[0].ProjectRoot != project {
		t.Fatalf("included configured project decoder panic: diagnostics=%+v err=%v", ds, err)
	}
}

type diagnosticPanicDecoder struct{ observedHookDecoder }

func (diagnosticPanicDecoder) DiagnosticProject(input agentapi.HookInput) string {
	root, _ := input.Payload["tree"].(string)
	return root
}

type brokenDiagnosticDecoder struct {
	observedHookDecoder
	root string
	fail bool
}

func (d brokenDiagnosticDecoder) DiagnosticProject(agentapi.HookInput) string {
	if d.fail {
		panic("diagnostic failure")
	}
	return d.root
}

type diagnosticCase string

const (
	diagnosticRelative    diagnosticCase = "relative"
	diagnosticInvalidUTF8 diagnosticCase = "invalid_utf8"
	diagnosticOversized   diagnosticCase = "oversized"
	diagnosticExcluded    diagnosticCase = "excluded"
	diagnosticNested      diagnosticCase = "nested"
	diagnosticAbsent      diagnosticCase = "absent"
)

func TestDecoderPanicDiagnosticRefusesInvalidRoots(t *testing.T) {
	for _, kind := range []diagnosticCase{diagnosticRelative, diagnosticInvalidUTF8, diagnosticOversized, diagnosticExcluded, diagnosticNested, diagnosticAbsent} {
		t.Run(string(kind), func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			env := testEnv(t, home, at)
			plain := observedHookDecoder{decode: func(agentapi.HookInput) []agentapi.LifecycleEvent { panic("original decoder failure") }}
			var decoder agentapi.HookDecoder = plain
			root := project
			switch kind {
			case diagnosticRelative:
				root = string(diagnosticRelative)
			case diagnosticInvalidUTF8:
				root = "/\xff"
			case diagnosticOversized:
				root = "/" + strings.Repeat("x", 16<<20)
			case diagnosticExcluded:
				root = t.TempDir()
			case diagnosticNested, diagnosticAbsent:
			}
			if kind != diagnosticAbsent {
				decoder = brokenDiagnosticDecoder{plain, root, kind == diagnosticNested}
			}
			env.Agents = registryWithSyntheticHooks(t, syntheticHooks{}, decoder)
			var stderr bytes.Buffer
			if code := runHookCommand([]string{"--harness", "native-synth"}, strings.NewReader(`{"opaque":"raw-secret","tree":`+quoteJSON(project)+`}`), &stderr, env); code != 0 || !strings.Contains(stderr.String(), "original decoder failure") {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			ds, err := capture.ReadDiagnostics(home)
			if err != nil || len(ds) != 0 {
				t.Fatalf("invalid diagnostic %+v %v", ds, err)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 0 {
				t.Fatalf("panic admitted %+v %v", regs, err)
			}
		})
	}
}
