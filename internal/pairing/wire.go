package pairing

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// Prefix identifies the encrypted beta wire format recognized by redaction.
const Prefix = "aa-pair1:"

// MaxBundle bounds encoded input before allocation or derivation.
const MaxBundle = 64 * 1024

// MaxPayload bounds authenticated decompression and JSON encoding.
const MaxPayload = 256 * 1024

const headerSize = 74

// Header is unauthenticated inspection data until Open authenticates it.
type Header struct {
	PairingID string
	ExpiresAt time.Time
}

func decode(bundle string) ([]byte, error) {
	if len(bundle) > MaxBundle || !strings.HasPrefix(bundle, Prefix) {
		return nil, errors.New("invalid or oversized pairing bundle; paste the complete aa-pair1 bundle")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(bundle, Prefix))
	if err != nil || len(b) < headerSize+chacha20poly1305.Overhead+4 {
		return nil, errors.New("pairing bundle is truncated or damaged; copy it again")
	}
	if crc32.ChecksumIEEE(b[:len(b)-4]) != binary.BigEndian.Uint32(b[len(b)-4:]) {
		return nil, errors.New("pairing bundle checksum failed; copy it again")
	}
	if b[0] != 1 {
		return nil, errors.New("unsupported pairing version; upgrade agent-archive")
	}
	if binary.BigEndian.Uint32(b[1:5]) != 3 || binary.BigEndian.Uint32(b[5:9]) != 65536 || b[9] != 4 {
		return nil, errors.New("unsupported pairing derivation parameters; upgrade agent-archive")
	}
	return b, nil
}

// Inspect validates bounded format and CRC before requesting a code. It does
// not authenticate expiry or provenance and never derives a key.
func Inspect(bundle string) (Header, error) {
	b, err := decode(bundle)
	if err != nil {
		return Header{}, err
	}
	return header(b), nil
}

func header(b []byte) Header {
	var seconds int64
	_, _ = binary.Decode(b[66:74], binary.BigEndian, &seconds)
	return Header{PairingID: hex.EncodeToString(b[50:66]), ExpiresAt: time.Unix(seconds, 0).UTC()}
}

// Seal encrypts a validated whitelist payload with fixed Argon2id and XChaCha.
func Seal(p Payload, code string) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	code, err := NormalizeCode(code)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if len(data) > MaxPayload {
		return "", errors.New("pairing settings exceed the payload limit")
	}
	var compressed bytes.Buffer
	z := gzip.NewWriter(&compressed)
	if _, err = z.Write(data); err != nil {
		return "", err
	}
	if err = z.Close(); err != nil {
		return "", err
	}
	b := make([]byte, headerSize)
	b[0] = 1
	binary.BigEndian.PutUint32(b[1:5], 3)
	binary.BigEndian.PutUint32(b[5:9], 65536)
	b[9] = 4
	if _, err = rand.Read(b[10:50]); err != nil {
		return "", err
	}
	id, _ := hex.DecodeString(p.PairingID)
	copy(b[50:66], id)
	_, _ = binary.Encode(b[66:74], binary.BigEndian, p.ExpiresAt.Unix())
	key := argon2.IDKey([]byte(code), b[10:26], 3, 65536, 4, 32)
	defer clear(key)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", err
	}
	b = aead.Seal(b, b[26:50], compressed.Bytes(), b)
	b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
	bundle := Prefix + base64.RawURLEncoding.EncodeToString(b)
	if len(bundle) > MaxBundle {
		return "", errors.New("pairing bundle exceeds the encoded limit")
	}
	return bundle, nil
}

// Open authenticates before checking expiry and enforces a decompression cap.
// Honest clients accept at most five minutes of destination clock skew.
func Open(bundle, code string, now time.Time) (Payload, error) {
	var p Payload
	b, err := decode(bundle)
	if err != nil {
		return p, err
	}
	code, err = NormalizeCode(code)
	if err != nil {
		return p, err
	}
	key := argon2.IDKey([]byte(code), b[10:26], 3, 65536, 4, 32)
	defer clear(key)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return p, err
	}
	data, err := aead.Open(nil, b[26:50], b[headerSize:len(b)-4], b[:headerSize])
	if err != nil {
		return p, errors.New("pairing code is wrong or the bundle was modified")
	}
	defer clear(data)
	h := header(b)
	if now.After(h.ExpiresAt.Add(5 * time.Minute)) {
		return p, fmt.Errorf("pairing expired %s ago by this machine's clock; create a new pairing", now.Sub(h.ExpiresAt).Round(time.Second))
	}
	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return p, errors.New("invalid paired settings compression")
	}
	defer func() { _ = z.Close() }()
	plain, err := io.ReadAll(io.LimitReader(z, MaxPayload+1))
	if err != nil || len(plain) > MaxPayload {
		return p, errors.New("paired settings exceed the decompression limit or are damaged")
	}
	defer clear(plain)
	if err = uniqueJSON(plain); err != nil {
		return Payload{}, errors.New("invalid or duplicate paired settings")
	}
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&p); err != nil {
		return Payload{}, errors.New("invalid paired settings")
	}
	var trailing any
	if err = dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Payload{}, errors.New("invalid trailing paired settings")
	}
	if err = p.Validate(); err != nil {
		return Payload{}, err
	}
	if p.PairingID != h.PairingID || p.ExpiresAt.Unix() != h.ExpiresAt.Unix() || p.CreatedAt.After(now.Add(5*time.Minute)) {
		return Payload{}, errors.New("pairing identity or times are inconsistent")
	}
	return p, nil
}
