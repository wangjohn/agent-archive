package archive

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Source bundle wire format, schema version 2: gzip of newline-delimited JSON.
// Every line is one object with a "kind" discriminator, in this order:
//
//  1. exactly one "header" line, first, carrying the envelope and the number
//     of each following kind of line;
//  2. one "native_record" line per retained native record, in source order;
//  3. one "native_text" line per retained native text transcript;
//  4. one "supplemental_evidence" line per evidence item.
//
// A reader can therefore stream a bundle one record at a time instead of
// holding the whole decompressed document, which schema 1 (one JSON document)
// required.
const (
	SourceLineHeader               = "header"
	SourceLineNativeRecord         = "native_record"
	SourceLineNativeText           = "native_text"
	SourceLineSupplementalEvidence = "supplemental_evidence"
)

// sourceRecordBytes bounds the size of one native record in a source bundle.
// It equals the record size limit PR D1 introduces as archive.MaxRecordBytes
// (the collector's maximum transcript size, 64 MiB); until that constant
// exists it is defined here, and should then be replaced by it.
const sourceRecordBytes = 64 << 20

// MaxSourceLineBytes bounds one decoded source line: the largest native
// record plus room for the line's own envelope and JSON escaping overhead.
const MaxSourceLineBytes = sourceRecordBytes + 1<<20

// SourceCounts records how many lines of each kind follow the header.
type SourceCounts struct {
	NativeRecords        int `json:"native_records"`
	NativeText           int `json:"native_text"`
	SupplementalEvidence int `json:"supplemental_evidence"`
}

// SourceHeader is the first line of a source bundle.
type SourceHeader struct {
	Kind             string                   `json:"kind"`
	SchemaVersion    int                      `json:"schema_version"`
	ArchiveSessionID string                   `json:"archive_session_id"`
	NativeSessionID  string                   `json:"native_session_id"`
	ProjectID        string                   `json:"project_id"`
	Capture          SourceCapture            `json:"capture"`
	ParentSessionID  string                   `json:"parent_session_id,omitempty"`
	LinkedSessions   []LinkedSessionReference `json:"linked_sessions,omitempty"`
	Counts           SourceCounts             `json:"counts"`
}

// SourceLine is one decoded line. Kind names which one field is set.
type SourceLine struct {
	Kind         string
	Header       *SourceHeader
	NativeRecord map[string]any
	NativeText   *TextTranscript
	Evidence     *SupplementalEvidence
}

type nativeRecordLine struct {
	Kind   string         `json:"kind"`
	Record map[string]any `json:"record"`
}

type nativeTextLine struct {
	Kind    string `json:"kind"`
	Format  string `json:"format"`
	Content string `json:"content"`
}

// Supplemental evidence has its own "kind" field (skill_inventory, ...), so
// it is nested under "evidence" rather than flattened beside the line's kind.
type evidenceLine struct {
	Kind     string               `json:"kind"`
	Evidence SupplementalEvidence `json:"evidence"`
}

// EncodeSource writes bundle as schema-2 JSONL to w, uncompressed. The output
// is deterministic: struct fields keep their declared order and map keys are
// sorted by encoding/json, so identical evidence always yields identical bytes.
func EncodeSource(w io.Writer, bundle SourceBundle) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	header := SourceHeader{
		Kind: SourceLineHeader, SchemaVersion: bundle.SchemaVersion,
		ArchiveSessionID: bundle.ArchiveSessionID, NativeSessionID: bundle.NativeSessionID, ProjectID: bundle.ProjectID,
		Capture: bundle.Capture, ParentSessionID: bundle.ParentSessionID, LinkedSessions: bundle.LinkedSessions,
		Counts: SourceCounts{NativeRecords: len(bundle.NativeRecords), NativeText: len(bundle.NativeText), SupplementalEvidence: len(bundle.SupplementalEvidence)},
	}
	write := func(value any) error {
		line, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode source line: %w", err)
		}
		if len(line) > MaxSourceLineBytes {
			return fmt.Errorf("source line of %d bytes exceeds the %d byte line limit", len(line), MaxSourceLineBytes)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
		return nil
	}
	if err := write(header); err != nil {
		return err
	}
	for _, record := range bundle.NativeRecords {
		if err := write(nativeRecordLine{Kind: SourceLineNativeRecord, Record: record}); err != nil {
			return err
		}
	}
	for _, text := range bundle.NativeText {
		if err := write(nativeTextLine{Kind: SourceLineNativeText, Format: text.Format, Content: text.Content}); err != nil {
			return err
		}
	}
	for _, evidence := range bundle.SupplementalEvidence {
		if err := write(evidenceLine{Kind: SourceLineSupplementalEvidence, Evidence: evidence}); err != nil {
			return err
		}
	}
	return nil
}

// DecodeOptions bounds a streaming decode. Zero values use the defaults.
type DecodeOptions struct {
	// MaxLineBytes caps one decompressed line (default MaxSourceLineBytes).
	MaxLineBytes int
	// MaxUncompressedBytes caps the whole decompressed stream (default: no
	// cap beyond the per-line one).
	MaxUncompressedBytes int64
}

// ErrSourceTooLarge is returned when a bundle's decompressed size exceeds
// DecodeOptions.MaxUncompressedBytes.
var ErrSourceTooLarge = errors.New("source exceeds uncompressed read limit")

// DecodeSource streams a gzip-compressed schema-2 source bundle, calling fn
// once per line in order. It holds at most one line in memory, and enforces
// the format's structure: the header is first and appears once, line kinds
// arrive in their fixed order, every line is well formed and within the line
// limit, and the counts in the header match the lines that follow. Any
// violation, a truncated stream, or an error from fn stops the decode. The
// caller is responsible for verifying the compressed bytes' size and hash
// before decoding.
func DecodeSource(compressed io.Reader, options DecodeOptions, fn func(SourceLine) error) error {
	maxLine := options.MaxLineBytes
	if maxLine <= 0 {
		maxLine = MaxSourceLineBytes
	}
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return fmt.Errorf("open source gzip: %w", err)
	}
	defer gz.Close()
	var plain io.Reader = gz
	counter := &countingReader{r: gz, limit: options.MaxUncompressedBytes}
	if options.MaxUncompressedBytes > 0 {
		plain = counter
	}
	scanner := bufio.NewScanner(plain)
	// Every line the encoder writes ends with a newline. A final line
	// without one is still handed over, flagged, so that a schema-1 bundle
	// (one JSON document, no trailing newline) is reported as the
	// unsupported version it is, and anything else as a truncated stream.
	unterminated := false
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			unterminated = true
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	initial := 64 << 10
	if initial > maxLine {
		initial = maxLine
	}
	scanner.Buffer(make([]byte, 0, initial), maxLine)

	var header *SourceHeader
	var seen SourceCounts
	stage := 0 // index into the kind order below
	order := map[string]int{SourceLineNativeRecord: 1, SourceLineNativeText: 2, SourceLineSupplementalEvidence: 3}
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Bytes()
		var probe struct {
			Kind          string `json:"kind"`
			SchemaVersion *int   `json:"schema_version"`
		}
		if unterminated {
			if lineNo == 1 && json.Unmarshal(raw, &probe) == nil && probe.Kind == "" && probe.SchemaVersion != nil {
				return fmt.Errorf("unsupported source schema version %d; this build reads schema %d only", *probe.SchemaVersion, SourceSchemaVersion)
			}
			if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("source is truncated: %w", err)
			}
			return fmt.Errorf("source is truncated: %w", errUnterminatedLine)
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			if lineNo == 1 {
				return fmt.Errorf("source line 1 is not a JSONL header (a schema 1 bundle is a single JSON document and is not supported): %w", err)
			}
			return fmt.Errorf("source line %d is not valid JSON: %w", lineNo, err)
		}
		if lineNo == 1 {
			if probe.Kind != SourceLineHeader {
				if probe.Kind == "" && probe.SchemaVersion != nil {
					return fmt.Errorf("unsupported source schema version %d; this build reads schema %d only", *probe.SchemaVersion, SourceSchemaVersion)
				}
				return fmt.Errorf("source line 1 is %q, but the header must come first", probe.Kind)
			}
			var decoded SourceHeader
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return fmt.Errorf("decode source header: %w", err)
			}
			if decoded.SchemaVersion != SourceSchemaVersion {
				return fmt.Errorf("unsupported source schema version %d; this build reads schema %d only", decoded.SchemaVersion, SourceSchemaVersion)
			}
			if decoded.Counts.NativeRecords < 0 || decoded.Counts.NativeText < 0 || decoded.Counts.SupplementalEvidence < 0 {
				return errors.New("source header has negative counts")
			}
			header = &decoded
			if err := fn(SourceLine{Kind: SourceLineHeader, Header: header}); err != nil {
				return err
			}
			continue
		}
		if probe.Kind == SourceLineHeader {
			return fmt.Errorf("source line %d is a second header", lineNo)
		}
		position, known := order[probe.Kind]
		if !known {
			return fmt.Errorf("source line %d has unknown kind %q", lineNo, probe.Kind)
		}
		if position < stage {
			return fmt.Errorf("source line %d (%s) is out of order", lineNo, probe.Kind)
		}
		stage = position
		line := SourceLine{Kind: probe.Kind}
		switch probe.Kind {
		case SourceLineNativeRecord:
			var decoded nativeRecordLine
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return fmt.Errorf("decode source line %d: %w", lineNo, err)
			}
			if len(decoded.Record) == 0 {
				return fmt.Errorf("source line %d has an empty native record", lineNo)
			}
			seen.NativeRecords++
			if seen.NativeRecords > header.Counts.NativeRecords {
				return fmt.Errorf("source has more native records than its header's %d", header.Counts.NativeRecords)
			}
			line.NativeRecord = decoded.Record
		case SourceLineNativeText:
			var decoded nativeTextLine
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return fmt.Errorf("decode source line %d: %w", lineNo, err)
			}
			seen.NativeText++
			if seen.NativeText > header.Counts.NativeText {
				return fmt.Errorf("source has more native text lines than its header's %d", header.Counts.NativeText)
			}
			line.NativeText = &TextTranscript{Format: decoded.Format, Content: decoded.Content}
		case SourceLineSupplementalEvidence:
			var decoded evidenceLine
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return fmt.Errorf("decode source line %d: %w", lineNo, err)
			}
			seen.SupplementalEvidence++
			if seen.SupplementalEvidence > header.Counts.SupplementalEvidence {
				return fmt.Errorf("source has more supplemental evidence than its header's %d", header.Counts.SupplementalEvidence)
			}
			line.Evidence = &decoded.Evidence
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		switch {
		case errors.Is(err, bufio.ErrTooLong):
			return fmt.Errorf("source line %d exceeds the %d byte line limit", lineNo+1, maxLine)
		case errors.Is(err, ErrSourceTooLarge):
			return err
		case errors.Is(err, io.ErrUnexpectedEOF):
			return fmt.Errorf("source is truncated: %w", err)
		}
		return fmt.Errorf("read source: %w", err)
	}
	if header == nil {
		return errors.New("source is empty: no header line")
	}
	if seen != header.Counts {
		return fmt.Errorf("source line counts %+v do not match its header's %+v", seen, header.Counts)
	}
	return nil
}

// ReadSourceBundle decodes a whole compressed source bundle into a
// SourceBundle, streaming it line by line. It is DecodeSource for callers
// that want the assembled bundle.
func ReadSourceBundle(compressed io.Reader, options DecodeOptions) (SourceBundle, error) {
	var bundle SourceBundle
	err := DecodeSource(compressed, options, func(line SourceLine) error {
		switch line.Kind {
		case SourceLineHeader:
			h := line.Header
			bundle = SourceBundle{
				SchemaVersion: h.SchemaVersion, ArchiveSessionID: h.ArchiveSessionID, NativeSessionID: h.NativeSessionID,
				ProjectID: h.ProjectID, Capture: h.Capture, ParentSessionID: h.ParentSessionID, LinkedSessions: h.LinkedSessions,
				NativeRecords: make([]map[string]any, 0, min(h.Counts.NativeRecords, 1<<16)),
			}
		case SourceLineNativeRecord:
			bundle.NativeRecords = append(bundle.NativeRecords, line.NativeRecord)
		case SourceLineNativeText:
			bundle.NativeText = append(bundle.NativeText, *line.NativeText)
		case SourceLineSupplementalEvidence:
			bundle.SupplementalEvidence = append(bundle.SupplementalEvidence, *line.Evidence)
		}
		return nil
	})
	if err != nil {
		return SourceBundle{}, err
	}
	return bundle, nil
}

// errUnterminatedLine reports a final line with no newline. The encoder ends
// every line with one, so such a line can only be the cut-off end of a
// truncated stream (or a schema-1 document), and it is refused, not parsed.
var errUnterminatedLine = errors.New("final line has no newline")

// countingReader fails once more than limit bytes have been read.
type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.limit > 0 && c.n > c.limit {
		return n, ErrSourceTooLarge
	}
	return n, err
}
