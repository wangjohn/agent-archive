package cursorstore

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
)

// rawHeaderIdentity preserves encoding/json's handling of malformed UTF-8 and
// surrogate escapes without returning the header's message content into Go.
func rawHeaderIdentity(ctx context.Context, q querier, key, name string, index int64) (string, error) {
	r := bufio.NewReader(&headerReader{ctx: ctx, q: q, key: key, name: name, index: index})
	if first, err := jsonNonspace(r); err != nil || first != '{' {
		return "", NotChecked(UnknownFormat)
	}
	var identity *string
	for {
		first, err := jsonNonspace(r)
		if err != nil {
			return "", err
		}
		if first == '}' {
			if identity == nil {
				return "", nil
			}
			return *identity, nil
		}
		if first != '"' {
			return "", NotChecked(UnknownFormat)
		}
		// Eight letters escaped as unicode occupy at most 50 raw bytes.
		field, err := jsonValue(r, first, 64)
		if err != nil {
			return "", err
		}
		var name string
		_ = json.Unmarshal(field, &name)
		if colon, err := jsonNonspace(r); err != nil || colon != ':' {
			return "", NotChecked(UnknownFormat)
		}
		first, err = jsonNonspace(r)
		if err != nil {
			return "", err
		}
		limit := 0
		if strings.EqualFold(name, "bubbleId") {
			limit = -1 // Native identity lengths remain unrestricted.
		}
		value, err := jsonValue(r, first, limit)
		if err != nil {
			return "", err
		}
		if limit != 0 && json.Unmarshal(value, &identity) != nil {
			return "", NotChecked(UnknownFormat)
		}
		next, err := jsonNonspace(r)
		if err != nil {
			return "", err
		}
		if next == '}' {
			if identity == nil {
				return "", nil
			}
			return *identity, nil
		}
		if next != ',' {
			return "", NotChecked(UnknownFormat)
		}
	}
}

type headerReader struct {
	ctx    context.Context
	q      querier
	key    string
	name   string
	index  int64
	offset int64
	chunk  []byte
}

func (r *headerReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(r.chunk) == 0 {
		err := r.q.QueryRowContext(r.ctx, `SELECT substr(CAST(j.value AS BLOB),?,65536) FROM json_each((SELECT value FROM json_each((SELECT value FROM cursorDiskKV WHERE key = ?)) WHERE key = ? ORDER BY id DESC LIMIT 1)) AS j WHERE j.id = ?`, r.offset+1, r.key, r.name, r.index).Scan(&r.chunk)
		if err != nil {
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

func jsonNonspace(r *bufio.Reader) (byte, error) {
	for {
		b, err := r.ReadByte()
		if err != nil || b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return b, err
		}
	}
}

// jsonValue skips a SQL-validated JSON value. Only selected identities and
// short object field names are retained; unknown content is never buffered.
func jsonValue(r *bufio.Reader, first byte, limit int) ([]byte, error) {
	var out []byte
	appendByte := func(b byte) {
		if limit < 0 || len(out) < limit {
			out = append(out, b)
		}
	}
	appendByte(first)
	depth := 0
	quoted := first == '"'
	escaped := false
	if first == '{' || first == '[' {
		depth = 1
	}
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if !quoted && depth == 0 && (b == ',' || b == '}' || b == ']') {
			return out, r.UnreadByte()
		}
		appendByte(b)
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
				if depth == 0 {
					return out, nil
				}
			}
			continue
		}
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return out, nil
			}
		}
	}
}
