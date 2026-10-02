package cursorstore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// signatureJSONFallback handles valid JSON beyond SQLite's nesting limit.
// Unknown native content is validated and discarded through fixed-size reads;
// only the existing unrestricted native identities and scalar timestamp survive.
func signatureJSONFallback(ctx context.Context, q querier, key, id string) (Signature, error) {
	p := signatureJSON{r: bufio.NewReader(&composerJSONReader{ctx: ctx, q: q, key: key})}
	first, err := jsonNonspace(p.r)
	if err != nil || first != '{' {
		return Signature{}, NotChecked(UnknownFormat)
	}
	var timestamp int64
	var timestampErr error
	var headers, inline signatureHeaders
	err = p.object(1, func(name string) error {
		first, err := jsonNonspace(p.r)
		if err != nil {
			return err
		}
		switch signatureField(name) {
		case signatureUpdatedAt:
			timestamp, timestampErr = 0, nil
			if first == 'n' || first == '-' || first >= '0' && first <= '9' {
				raw, err := p.scalar(first, true)
				if err != nil {
					return err
				}
				var number float64
				timestampErr = json.Unmarshal(raw, &number)
				timestamp = int64(number)
				return nil
			}
			timestampErr = NotChecked(UnknownFormat)
			return p.skip(first, 1)
		case signatureConversationHeaders:
			headers, err = p.headers(first, false)
			return err
		case signatureConversation:
			inline, err = p.headers(first, true)
			return err
		default:
			return p.skip(first, 1)
		}
	})
	if err != nil {
		return Signature{}, signatureJSONError(err)
	}
	if _, err := jsonNonspace(p.r); !errors.Is(err, io.EOF) {
		return Signature{}, signatureJSONError(err)
	}
	if timestampErr != nil {
		return Signature{}, NotChecked(UnknownFormat)
	}
	selected := headers
	if !headers.present {
		selected = inline
	}
	if selected.err != nil {
		return Signature{}, NotChecked(UnknownFormat)
	}
	sig := Signature{LastUpdatedAt: timestamp, HeaderCount: selected.count, LastBubbleID: selected.last}
	if headers.present {
		return signatureRows(ctx, q, id, sig, selected.ids)
	}
	return sig, nil
}

func signatureJSONError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return NotChecked(UnknownFormat)
	}
	return err
}

type composerJSONReader struct {
	ctx    context.Context
	q      querier
	key    string
	offset int64
	chunk  []byte
}

func (r *composerJSONReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(r.chunk) == 0 {
		if err := r.q.QueryRowContext(r.ctx, `SELECT substr(CAST(value AS BLOB),?,65536) FROM cursorDiskKV WHERE key = ?`, r.offset+1, r.key).Scan(&r.chunk); err != nil {
			return 0, err
		}
		if len(r.chunk) == 0 {
			return 0, io.EOF
		}
		r.offset += int64(len(r.chunk))
	}
	n := copy(p, r.chunk)
	r.chunk = r.chunk[n:]
	return n, nil
}

type signatureJSON struct{ r *bufio.Reader }

type signatureField string

const (
	signatureUpdatedAt           signatureField = "lastUpdatedAt"
	signatureConversationHeaders signatureField = "fullConversationHeadersOnly"
	signatureConversation        signatureField = "conversation"
)

type signatureHeaders struct {
	present bool
	count   int
	last    string
	ids     []string
	err     error
}

func (p *signatureJSON) headers(first byte, inline bool) (out signatureHeaders, err error) {
	if first == 'n' {
		_, err = p.scalar(first, false)
		return out, err
	}
	out.present = true
	if first != '[' {
		out.err = NotChecked(UnknownFormat)
		return out, p.skip(first, 1)
	}
	err = p.array(2, func(first byte) error {
		var identity *string
		invalid := false
		switch first {
		case '{':
			if err := p.object(3, func(name string) error {
				first, err := jsonNonspace(p.r)
				if err != nil {
					return err
				}
				if !strings.EqualFold(name, "bubbleId") {
					return p.skip(first, 3)
				}
				if first == '"' {
					raw, err := p.string(-1)
					if err != nil {
						return err
					}
					var value string
					if err := json.Unmarshal(raw, &value); err != nil {
						return NotChecked(UnknownFormat)
					}
					identity = &value
					return nil
				}
				if first == 'n' {
					identity = nil
					_, err := p.scalar(first, false)
					return err
				}
				invalid = true
				return p.skip(first, 3)
			}); err != nil {
				return err
			}
		case 'n':
			if _, err := p.scalar(first, false); err != nil {
				return err
			}
		default:
			invalid = true
			if err := p.skip(first, 2); err != nil {
				return err
			}
		}
		out.count++
		out.last = ""
		if identity != nil {
			out.last = *identity
		}
		if invalid || !inline && (identity == nil || *identity == "") {
			out.err = NotChecked(UnknownFormat)
		}
		if !inline {
			out.ids = append(out.ids, out.last)
		}
		return nil
	})
	return out, err
}

// object starts immediately after '{'; visit consumes exactly one field value.
func (p *signatureJSON) object(depth int, visit func(string) error) error {
	if depth > 10000 { // encoding/json's existing maximum JSON nesting depth.
		return NotChecked(UnknownFormat)
	}
	first, err := jsonNonspace(p.r)
	if err != nil || first == '}' {
		return err
	}
	for {
		if first != '"' {
			return NotChecked(UnknownFormat)
		}
		raw, err := p.string(128)
		if err != nil {
			return err
		}
		var name string
		// Longer field names cannot equal any selected native field. The
		// string has been fully validated even when its bytes are discarded.
		if raw != nil {
			if err := json.Unmarshal(raw, &name); err != nil {
				return NotChecked(UnknownFormat)
			}
		}
		if colon, err := jsonNonspace(p.r); err != nil || colon != ':' {
			return signatureJSONError(err)
		}
		if err := visit(name); err != nil {
			return err
		}
		first, err = jsonNonspace(p.r)
		if err != nil || first == '}' {
			return err
		}
		if first != ',' {
			return NotChecked(UnknownFormat)
		}
		first, err = jsonNonspace(p.r)
		if err != nil {
			return err
		}
	}
}

func (p *signatureJSON) array(depth int, visit func(byte) error) error {
	if depth > 10000 {
		return NotChecked(UnknownFormat)
	}
	first, err := jsonNonspace(p.r)
	if err != nil || first == ']' {
		return err
	}
	for {
		if err := visit(first); err != nil {
			return err
		}
		first, err = jsonNonspace(p.r)
		if err != nil || first == ']' {
			return err
		}
		if first != ',' {
			return NotChecked(UnknownFormat)
		}
		first, err = jsonNonspace(p.r)
		if err != nil {
			return err
		}
	}
}

func (p *signatureJSON) skip(first byte, depth int) error {
	switch first {
	case '{':
		return p.object(depth+1, func(string) error {
			first, err := jsonNonspace(p.r)
			if err != nil {
				return err
			}
			return p.skip(first, depth+1)
		})
	case '[':
		return p.array(depth+1, func(first byte) error { return p.skip(first, depth+1) })
	case '"':
		_, err := p.string(0)
		return err
	default:
		_, err := p.scalar(first, false)
		return err
	}
}

// string validates escapes and control bytes without retaining unknown content.
// The opening quote has already been consumed.
func (p *signatureJSON) string(limit int) ([]byte, error) {
	var out []byte
	retained := true
	add := func(b byte) {
		if limit < 0 || len(out) < limit {
			out = append(out, b)
		} else {
			retained = false
		}
	}
	add('"')
	for {
		b, err := p.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b < ' ' {
			return nil, NotChecked(UnknownFormat)
		}
		add(b)
		if b == '"' {
			if !retained {
				return nil, nil
			}
			return out, nil
		}
		if b != '\\' {
			continue
		}
		b, err = p.r.ReadByte()
		if err != nil {
			return nil, err
		}
		add(b)
		if b == 'u' {
			for range 4 {
				b, err = p.r.ReadByte()
				if err != nil {
					return nil, err
				}
				if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
					return nil, NotChecked(UnknownFormat)
				}
				add(b)
			}
		} else if !strings.ContainsRune(`"\/bfnrt`, rune(b)) {
			return nil, NotChecked(UnknownFormat)
		}
	}
}

func (p *signatureJSON) scalar(first byte, retain bool) ([]byte, error) {
	if word := jsonLiteralWord(first); word != "" {
		return p.literal(word, retain)
	}
	var number signatureNumber
	add := func(b byte) {
		if retain {
			number.add(b)
		}
	}
	add(first)
	if first == '-' {
		var err error
		first, err = p.r.ReadByte()
		if err != nil {
			return nil, err
		}
		add(first)
	}
	if first < '0' || first > '9' {
		return nil, NotChecked(UnknownFormat)
	}
	digits := func(required bool) error {
		n := 0
		for {
			b, err := p.r.Peek(1)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if b[0] < '0' || b[0] > '9' {
				break
			}
			_, _ = p.r.ReadByte()
			add(b[0])
			n++
		}
		if required && n == 0 {
			return NotChecked(UnknownFormat)
		}
		return nil
	}
	if first != '0' {
		if err := digits(false); err != nil {
			return nil, err
		}
	}
	if b, err := p.r.Peek(1); err == nil && b[0] == '.' {
		_, _ = p.r.ReadByte()
		add('.')
		if err := digits(true); err != nil {
			return nil, err
		}
	}
	if b, err := p.r.Peek(1); err == nil && (b[0] == 'e' || b[0] == 'E') {
		_, _ = p.r.ReadByte()
		add(b[0])
		if sign, err := p.r.Peek(1); err == nil && (sign[0] == '+' || sign[0] == '-') {
			_, _ = p.r.ReadByte()
			add(sign[0])
		}
		if err := digits(true); err != nil {
			return nil, err
		}
	}
	if !retain {
		return nil, nil
	}
	return number.token(), nil
}

// Float64 rounding midpoints have fewer than 1100 significant decimal digits.
// Retaining that prefix and one sticky digit preserves ParseFloat rounding,
// including a value just beyond a midpoint, without buffering huge zero runs.
type signatureNumber struct {
	digits       []byte
	positions    int64
	integers     int64
	first        int64
	exponent     int64
	fraction     bool
	negative     bool
	inExponent   bool
	exponentSign bool
	sticky       bool
}

func (n *signatureNumber) add(b byte) {
	switch b {
	case '-':
		if n.inExponent {
			n.exponentSign = true
		} else {
			n.negative = true
		}
	case '.':
		n.fraction = true
	case 'e', 'E':
		n.inExponent = true
	default:
		if b < '0' || b > '9' {
			return
		}
		if n.inExponent {
			// Native SQLite values cannot contain enough digits to cancel
			// an exponent this large; saturation preserves overflow/underflow.
			if n.exponent <= (1<<60)/10 {
				n.exponent = min(n.exponent*10+int64(b-'0'), 1<<60)
			} else {
				n.exponent = 1 << 60
			}
			return
		}
		if !n.fraction {
			n.integers++
		}
		if len(n.digits) == 0 && b == '0' {
			n.positions++
			return
		}
		if len(n.digits) == 0 {
			n.first = n.positions
		}
		n.positions++
		if len(n.digits) < 1100 {
			n.digits = append(n.digits, b)
		} else if b != '0' {
			n.sticky = true
		}
	}
}

func (n *signatureNumber) token() []byte {
	if len(n.digits) == 0 {
		return []byte("0")
	}
	var token []byte
	if n.negative {
		token = append(token, '-')
	}
	token = append(token, n.digits[0], '.')
	token = append(token, n.digits[1:]...)
	// Always include a fractional digit for a one-digit mantissa.
	if n.sticky {
		token = append(token, '1')
	} else {
		token = append(token, '0')
	}
	exponent := n.exponent
	if n.exponentSign {
		exponent = -exponent
	}
	exponent += n.integers - n.first - 1
	token = append(token, 'e')
	return strconv.AppendInt(token, exponent, 10)
}

func jsonLiteralWord(first byte) string {
	switch first {
	case 'n':
		return "null"
	case 't':
		return "true"
	case 'f':
		return "false"
	default:
		return ""
	}
}

func (p *signatureJSON) literal(word string, retain bool) ([]byte, error) {
	for i := 1; i < len(word); i++ {
		b, err := p.r.ReadByte()
		if err != nil || b != word[i] {
			return nil, signatureJSONError(err)
		}
	}
	if retain {
		return []byte(word), nil
	}
	return nil, nil
}
