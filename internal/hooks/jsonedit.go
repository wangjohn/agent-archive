package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// The hook files setup edits belong to the user and to their applications,
// which rewrite them too. Setup therefore never round-trips a whole file
// through a Go map: that would sort every key, HTML-escape strings (every
// "&" and "<" becoming a \u escape) and round large integers through
// float64. A document instead keeps the file's bytes and splices in only
// the top-level members setup owns ("hooks", and Cursor's "version");
// everything else, formatting included, is left byte for byte. The spliced
// value is encoded from an order-preserving tree (object, below) with the
// file's own indentation, numbers kept as their literal text and HTML
// escaping off, so a user's own handlers inside "hooks" also keep their key
// order and values.

// object is a JSON object that keeps its members in file order.
type object struct{ members []member }

type member struct {
	key   string
	value any
}

// get returns the value of key. JSON parsers conventionally keep the last of
// duplicate keys, and so does get.
func (o *object) get(key string) (any, bool) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.members[i].key == key {
			return o.members[i].value, true
		}
	}
	return nil, false
}

// set replaces every member named key with one holding value, in the place
// of the first, or appends it.
func (o *object) set(key string, value any) {
	for i := range o.members {
		if o.members[i].key == key {
			o.members[i].value = value
			o.remove(key, i+1)
			return
		}
	}
	o.members = append(o.members, member{key, value})
}

// remove deletes every member named key at or after index from.
func (o *object) remove(key string, from int) {
	kept := o.members[:from]
	for _, m := range o.members[from:] {
		if m.key != key {
			kept = append(kept, m)
		}
	}
	o.members = kept
}

// decodeValue reads one JSON value from dec, which must have UseNumber set:
// objects become *object, arrays []any, numbers json.Number.
func decodeValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		o := &object{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			value, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			o.members = append(o.members, member{key, value})
		}
		_, err = dec.Token()
		return o, err
	case '[':
		list := []any{}
		for dec.More() {
			value, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, value)
		}
		_, err = dec.Token()
		return list, err
	}
	return nil, errors.New("unexpected JSON delimiter")
}

// encodeValue writes value as compact JSON without HTML escaping.
func encodeValue(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(v.String())
	case string:
		return encodeString(buf, v)
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeValue(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case *object:
		buf.WriteByte('{')
		for i, m := range v.members {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeString(buf, m.key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := encodeValue(buf, m.value); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return errors.New("unsupported JSON value")
	}
	return nil
}

func encodeString(buf *bytes.Buffer, s string) error {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Write(bytes.TrimSuffix(out.Bytes(), []byte("\n")))
	return nil
}

// document is a JSON file whose top level is an object, with the byte span
// of each top-level member so single members can be replaced, added, or
// removed without touching the rest of the file.
type document struct {
	src     []byte
	root    *object
	spans   []memberSpan // one per top-level member, in file order
	open    int          // offset of the root '{'
	close   int          // offset of the root '}'
	indent  string       // whitespace before each top-level key; "" when compact
	newline string       // the file's line ending, "\r\n" or "\n"
	created bool         // the file was empty, so the whole document is new
	edits   []edit
}

type memberSpan struct {
	key      string
	keyStart int
	valStart int
	valEnd   int
}

type edit struct {
	start int
	end   int
	text  string
}

var errInvalidConfiguration = errors.New("invalid existing hook configuration")

// invalid is errInvalidConfiguration for src, saying where and, for the
// forms people most often put in these files by accident, what: a
// byte-order mark, a comment (JSONC), or a trailing comma, none of which is
// JSON. The applications themselves read the files as plain JSON.
func invalid(src []byte, err error) error {
	if bytes.HasPrefix(src, []byte("\xef\xbb\xbf")) {
		return fmt.Errorf("%w: the file starts with a byte-order mark (BOM), which JSON does not allow; save it as UTF-8 without one", errInvalidConfiguration)
	}
	var syntaxErr *json.SyntaxError
	offset := -1
	switch {
	case errors.As(err, &syntaxErr):
		offset = int(syntaxErr.Offset) - 1
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		offset = len(src)
	}
	if offset < 0 || offset > len(src) {
		return fmt.Errorf("%w: %v", errInvalidConfiguration, err)
	}
	// Offset counts the bytes read, which ends just after the one that
	// failed; step back over whitespace to the character itself.
	for offset > 0 && offset < len(src) && (src[offset] == ' ' || src[offset] == '\n' || src[offset] == '\r' || src[offset] == '\t') {
		offset--
	}
	line := 1 + bytes.Count(src[:offset], []byte("\n"))
	column := offset - bytes.LastIndexByte(src[:offset], '\n')
	hint := ""
	rest := src[offset:]
	after := bytes.TrimLeft(bytes.TrimPrefix(rest, []byte(",")), " \t\r\n")
	switch {
	case bytes.HasPrefix(rest, []byte("//")) || bytes.HasPrefix(rest, []byte("/*")):
		hint = "; comments (JSONC) are not JSON, so remove them"
	case bytes.HasPrefix(rest, []byte(",")) && (bytes.HasPrefix(after, []byte("}")) || bytes.HasPrefix(after, []byte("]"))):
		hint = "; a comma before a closing brace or bracket (a trailing comma) is not JSON, so remove it"
	}
	return fmt.Errorf("%w: line %d, column %d: %v%s", errInvalidConfiguration, line, column, err, hint)
}

// parseDocument reads src, which must be empty or a single JSON object.
func parseDocument(src []byte) (*document, error) {
	newline := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		newline = "\r\n"
	}
	created := len(bytes.TrimSpace(src)) == 0
	var indent string
	if created {
		src, indent = []byte("{}"), "  "
	}
	d := &document{
		src:     src,
		newline: newline,
		created: created,
		indent:  indent,
	}
	dec := json.NewDecoder(bytes.NewReader(d.src))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return nil, invalid(d.src, err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("%w: the file must hold one JSON object", errInvalidConfiguration)
	}
	d.open = int(dec.InputOffset()) - 1
	d.root = &object{}
	for dec.More() {
		before := int(dec.InputOffset())
		keyToken, err := dec.Token()
		if err != nil {
			return nil, invalid(d.src, err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, invalid(d.src, err)
		}
		// Setup would edit one of two members that tools resolve
		// differently (the last wins in Go and JavaScript, not everywhere),
		// so a duplicate of a member it owns is refused.
		//lint:ignore LV1001 top-level member names of a user's JSON file are an open set; only these two are setup's
		if _, dup := d.span(key); dup && (key == "hooks" || key == "version") {
			return nil, fmt.Errorf("%w: more than one top-level %q key; remove the duplicate", errInvalidConfiguration, key)
		}
		keyStart := skipUntil(d.src, before, '"')
		valStart := skipSpace(d.src, skipSpace(d.src, int(dec.InputOffset()))+1)
		value, err := decodeValue(dec)
		if err != nil {
			return nil, invalid(d.src, err)
		}
		d.root.members = append(d.root.members, member{key, value})
		d.spans = append(d.spans, memberSpan{key, keyStart, valStart, int(dec.InputOffset())})
	}
	if token, err = dec.Token(); err != nil {
		return nil, invalid(d.src, err)
	} else if token != json.Delim('}') {
		return nil, fmt.Errorf("%w: the file must hold one JSON object", errInvalidConfiguration)
	}
	d.close = int(dec.InputOffset()) - 1
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: more than one JSON value in the file", errInvalidConfiguration)
	}
	if !d.created {
		d.indent = d.memberIndent()
	}
	return d, nil
}

// memberIndent is the whitespace a top-level key is indented with: that of
// the first member that starts a line of its own, so a file whose first key
// shares the opening brace's line ({"a": 1,\n  "b": 2}) still gets its
// new members on lines of their own. It is "" for a compact file (no member
// starts a line), and two spaces for an empty object.
func (d *document) memberIndent() string {
	if len(d.spans) == 0 {
		return "  "
	}
	start := d.open + 1
	for _, s := range d.spans {
		lead := string(d.src[start:s.keyStart])
		// Only a key that starts its line counts: "\n  ," puts the
		// separator there, which is no indentation.
		if i := strings.LastIndexByte(lead, '\n'); i >= 0 && strings.Trim(lead[i+1:], " \t") == "" {
			return lead[i+1:]
		}
		start = s.valEnd
	}
	return ""
}

func skipSpace(src []byte, i int) int {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i++
	}
	return i
}

func skipUntil(src []byte, i int, b byte) int {
	for i < len(src) && src[i] != b {
		i++
	}
	return i
}

// render encodes value as it would appear as a top-level member's value.
func (d *document) render(value any) (string, error) {
	var compact bytes.Buffer
	if err := encodeValue(&compact, value); err != nil {
		return "", err
	}
	if d.indent == "" {
		return compact.String(), nil
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact.Bytes(), d.indent, d.indent); err != nil {
		return "", err
	}
	// JSON strings cannot hold a raw newline, so every one here is layout.
	return strings.ReplaceAll(out.String(), "\n", d.newline), nil
}

func (d *document) span(key string) (int, bool) {
	for i := len(d.spans) - 1; i >= 0; i-- {
		if d.spans[i].key == key {
			return i, true
		}
	}
	return 0, false
}

// set replaces the value of a top-level member in place, or adds the member
// after the last one.
func (d *document) set(key string, value any) error {
	text, err := d.render(value)
	if err != nil {
		return err
	}
	d.root.set(key, value)
	if i, ok := d.span(key); ok {
		d.edits = append(d.edits, edit{d.spans[i].valStart, d.spans[i].valEnd, text})
		return nil
	}
	var name bytes.Buffer
	if err := encodeString(&name, key); err != nil {
		return err
	}
	if d.indent == "" {
		entry := name.String() + ":" + text
		if len(d.spans) == 0 {
			d.edits = append(d.edits, edit{d.close, d.close, entry})
		} else {
			d.edits = append(d.edits, edit{d.spans[len(d.spans)-1].valEnd, d.spans[len(d.spans)-1].valEnd, "," + entry})
		}
		return nil
	}
	entry := d.newline + d.indent + name.String() + ": " + text
	if len(d.spans) > 0 {
		end := d.spans[len(d.spans)-1].valEnd
		d.edits = append(d.edits, edit{end, end, "," + entry})
		return nil
	}
	// An empty root object: lay it out afresh, keeping any members added
	// before this one in the same edit.
	if n := len(d.edits); n > 0 && d.edits[n-1].start == d.open+1 && d.edits[n-1].end == d.close {
		d.edits[n-1].text = strings.TrimSuffix(d.edits[n-1].text, d.newline) + "," + entry + d.newline
		return nil
	}
	d.edits = append(d.edits, edit{d.open + 1, d.close, entry + d.newline})
	return nil
}

// setBefore adds the member key just before the existing member before, or
// like set when there is no such member. key must not exist yet.
func (d *document) setBefore(before, key string, value any) error {
	i, ok := d.span(before)
	if !ok {
		return d.set(key, value)
	}
	text, err := d.render(value)
	if err != nil {
		return err
	}
	var name bytes.Buffer
	if err := encodeString(&name, key); err != nil {
		return err
	}
	for j, m := range d.root.members {
		if m.key == before {
			d.root.members = append(d.root.members[:j], append([]member{{key, value}}, d.root.members[j:]...)...)
			break
		}
	}
	entry := name.String() + ":" + text + ","
	if d.indent != "" {
		entry = name.String() + ": " + text + "," + d.newline + d.indent
	}
	d.edits = append(d.edits, edit{d.spans[i].keyStart, d.spans[i].keyStart, entry})
	return nil
}

// remove deletes a top-level member together with the separator before it
// (or, for the first member, the one after it), so a member set added is
// removed without a trace.
func (d *document) remove(key string) {
	i, ok := d.span(key)
	if !ok {
		return
	}
	d.root.remove(key, 0)
	s := d.spans[i]
	switch {
	case i > 0:
		d.edits = append(d.edits, edit{d.spans[i-1].valEnd, s.valEnd, ""})
	case len(d.spans) > 1:
		d.edits = append(d.edits, edit{s.keyStart, d.spans[1].keyStart, ""})
	default:
		d.edits = append(d.edits, edit{d.open + 1, d.close, ""})
	}
}

// clear empties the root object, replacing every edit made so far.
func (d *document) clear() {
	d.root.members = nil
	d.edits = []edit{{d.open + 1, d.close, ""}}
}

// bytes applies the edits. Edits never overlap: each touches one member.
// Two insertions at one offset keep the order they were made in: the later
// is applied first, so it ends up after the earlier.
func (d *document) bytes() []byte {
	edits := make([]edit, 0, len(d.edits))
	for i := len(d.edits) - 1; i >= 0; i-- {
		edits = append(edits, d.edits[i])
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), d.src...)
	for _, e := range edits {
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	if d.created {
		out = append(out, '\n')
	}
	return out
}
