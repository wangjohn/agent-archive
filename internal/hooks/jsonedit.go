package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
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
	created bool         // the file was empty, so the whole document is new
	edits   []edit
}

type memberSpan struct {
	key                        string
	keyStart, valStart, valEnd int
}

type edit struct {
	start, end int
	text       string
}

var errInvalidConfiguration = errors.New("invalid existing hook configuration")

// parseDocument reads src, which must be empty or a single JSON object.
func parseDocument(src []byte) (*document, error) {
	d := &document{src: src}
	if len(bytes.TrimSpace(src)) == 0 {
		d.src, d.created, d.indent = []byte("{}"), true, "  "
	}
	dec := json.NewDecoder(bytes.NewReader(d.src))
	dec.UseNumber()
	token, err := dec.Token()
	if delim, ok := token.(json.Delim); err != nil || !ok || delim != '{' {
		return nil, errInvalidConfiguration
	}
	d.open = int(dec.InputOffset()) - 1
	d.root = &object{}
	for dec.More() {
		before := int(dec.InputOffset())
		keyToken, err := dec.Token()
		if err != nil {
			return nil, errInvalidConfiguration
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errInvalidConfiguration
		}
		keyStart := skipUntil(d.src, before, '"')
		valStart := skipSpace(d.src, skipSpace(d.src, int(dec.InputOffset()))+1)
		value, err := decodeValue(dec)
		if err != nil {
			return nil, errInvalidConfiguration
		}
		d.root.members = append(d.root.members, member{key, value})
		d.spans = append(d.spans, memberSpan{key, keyStart, valStart, int(dec.InputOffset())})
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') {
		return nil, errInvalidConfiguration
	}
	d.close = int(dec.InputOffset()) - 1
	if _, err = dec.Token(); err != io.EOF {
		return nil, errInvalidConfiguration
	}
	if !d.created {
		d.indent = "  "
		if len(d.spans) > 0 {
			lead := string(d.src[d.open+1 : d.spans[0].keyStart])
			if i := strings.LastIndexByte(lead, '\n'); i >= 0 {
				d.indent = lead[i+1:]
			} else {
				d.indent = ""
			}
		}
	}
	return d, nil
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
	return out.String(), nil
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
	entry := "\n" + d.indent + name.String() + ": " + text
	if len(d.spans) > 0 {
		end := d.spans[len(d.spans)-1].valEnd
		d.edits = append(d.edits, edit{end, end, "," + entry})
		return nil
	}
	// An empty root object: lay it out afresh, keeping any members added
	// before this one in the same edit.
	if n := len(d.edits); n > 0 && d.edits[n-1].start == d.open+1 && d.edits[n-1].end == d.close {
		d.edits[n-1].text = strings.TrimSuffix(d.edits[n-1].text, "\n") + "," + entry + "\n"
		return nil
	}
	d.edits = append(d.edits, edit{d.open + 1, d.close, entry + "\n"})
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

// bytes applies the edits. Edits never overlap: each touches one member.
func (d *document) bytes() []byte {
	edits := append([]edit(nil), d.edits...)
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
