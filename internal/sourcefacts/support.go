package sourcefacts

import "encoding/json"

type codexProducerSource string

type codexProducerOriginator string

type codexProducerVersion string

const (
	codexProducerCLI       codexProducerSource     = "cli"
	codexProducerExec      codexProducerSource     = "exec"
	codexProducerDesktop   codexProducerSource     = "vscode"
	codexOriginatorTUI     codexProducerOriginator = "codex-tui"
	codexOriginatorExec    codexProducerOriginator = "codex_exec"
	codexOriginatorDesktop codexProducerOriginator = "Codex Desktop"
	codexVersion159        codexProducerVersion    = "0.159.3"
	codexVersion160        codexProducerVersion    = "0.160.0"
)

type codexProducer struct {
	Source     codexProducerSource
	Originator codexProducerOriginator
	Version    codexProducerVersion
}

// Exact producer tuples are a source-format compatibility boundary. Version
// 0.159.3 has released CLI/app-server probe evidence; 0.160.0 has pinned source
// compatibility evidence. Neither establishes desktop GUI acceptance or where
// an indistinguishable native copy originally executed.
var supportedCodexProducers = map[codexProducer]bool{
	{codexProducerCLI, codexOriginatorTUI, codexVersion159}:         true,
	{codexProducerExec, codexOriginatorExec, codexVersion159}:       true,
	{codexProducerDesktop, codexOriginatorDesktop, codexVersion159}: true,
	{codexProducerCLI, codexOriginatorTUI, codexVersion160}:         true,
	{codexProducerExec, codexOriginatorExec, codexVersion160}:       true,
	{codexProducerDesktop, codexOriginatorDesktop, codexVersion160}: true,
}

// SupportedCodexProducer recognizes inspected source formats. It does not
// attest local originating execution; indistinguishable recent copies qualify
// only after ordinary project/destination/native-creation consent checks.
func SupportedCodexProducer(m CodexMeta) bool {
	if m.Classification() != "native_format" {
		return false
	}
	var source codexProducerSource
	if json.Unmarshal(m.Source, &source) != nil {
		return false
	}
	return supportedCodexProducers[codexProducer{source, codexProducerOriginator(m.Originator), codexProducerVersion(m.Version)}]
}
