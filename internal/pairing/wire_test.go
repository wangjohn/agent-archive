package pairing

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"hash/crc32"
	"strings"
	"testing"
	"time"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func testPayload() Payload {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return Payload{Version: 1, PairingID: strings.Repeat("1", 32), RecipientID: strings.Repeat("2", 32), IssuerID: strings.Repeat("3", 32), Name: "laptop", IssuerName: "studio", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute), Storage: Storage{Provider: "s3", Bucket: "synthetic", Prefix: "archive/", AWSProfile: "archive", Region: "us-east-1"}, Apps: []string{"claude"}, RetentionDays: 90, SkillEvidence: "metadata", Inclusions: []Inclusion{{ID: strings.Repeat("4", 32), Label: "app", HomePath: "src/app"}}}
}

func TestWordlistProvenanceAndUniquePrefixes(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte(wordlist))
	if hex.EncodeToString(sum[:]) != "22b45c52e0bd0bbf03aa522240b111eb4c7c0c1d86c4e518e1be2a7eb2a625e4" {
		t.Fatal("wordlist hash changed")
	}
	list := words()
	if len(list) != 1296 {
		t.Fatalf("words %d", len(list))
	}
	seen := map[string]bool{}
	for _, word := range list {
		if len(word) < 3 || seen[word[:3]] {
			t.Fatalf("nonunique prefix for %s", word)
		}
		seen[word[:3]] = true
	}
}

func TestCodeAcceptsFullWordsAndPrefixes(t *testing.T) {
	t.Parallel()
	code, err := NewCode()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(code, "-")
	for i := range parts {
		parts[i] = strings.ToUpper(parts[i][:3])
	}
	normalized, err := NormalizeCode(strings.Join(parts, " "))
	if err != nil || normalized != code {
		t.Fatalf("normalized %q err %v", normalized, err)
	}
	if _, err = NormalizeCode("xxx aaa aaa aaa aaa aaa"); err == nil {
		t.Fatal("invalid prefix accepted")
	}
}

func TestWireRoundTripWrongCodeExpiryAndAuthenticatedHeader(t *testing.T) {
	// KDF tests stay sequential to bound simultaneous 64 MiB derivations.
	p := testPayload()
	code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	bundle, err := Seal(p, code)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Inspect(bundle)
	if err != nil || h.PairingID != p.PairingID || h.ExpiresAt != p.ExpiresAt {
		t.Fatalf("header %+v err %v", h, err)
	}
	got, err := Open(bundle, "aar aba abb abd abh abi", p.CreatedAt)
	if err != nil || got.Storage != p.Storage || got.PairingID != p.PairingID {
		t.Fatalf("got %+v err %v", got, err)
	}
	if _, err = Open(bundle, "aardvark-abandoned-abbreviate-abdomen-abhorrence-abnormal", p.CreatedAt); err == nil {
		t.Fatal("wrong code accepted")
	}
	if _, err = Open(bundle, code, p.ExpiresAt.Add(5*time.Minute)); err != nil {
		t.Fatal("skew allowance rejected", err)
	}
	if _, err = Open(bundle, code, p.ExpiresAt.Add(5*time.Minute+time.Second)); err == nil {
		t.Fatal("expired bundle accepted")
	}
	for _, offset := range []int{0, 1, 5, 9, 10, 26, 50, 66, headerSize} {
		b, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(bundle, Prefix))
		b[offset] ^= 1
		binary.BigEndian.PutUint32(b[len(b)-4:], crc32.ChecksumIEEE(b[:len(b)-4]))
		modified := Prefix + base64.RawURLEncoding.EncodeToString(b)
		if _, err = Open(modified, code, p.CreatedAt); err == nil {
			t.Fatalf("tampered offset%d accepted", offset)
		}
	}
}

func TestWireRejectsInputLimitsBeforeDerivation(t *testing.T) {
	t.Parallel()
	for _, bundle := range []string{"", Prefix + "abcd", Prefix + strings.Repeat("A", MaxBundle), "future:abc"} {
		if _, err := Inspect(bundle); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	p := testPayload()
	p.Inclusions[0].HomePath = "../escape"
	if err := p.Validate(); err == nil {
		t.Fatal("traversal accepted")
	}
	p = testPayload()
	p.HandoffArgs = map[string][]string{"codex": {"--flag\x00"}}
	if err := p.Validate(); err == nil {
		t.Fatal("NUL argument accepted")
	}
	p = testPayload()
	p.Storage.Provider = "s3"
	p.SecretAccessKey = "synthetic-secret"
	if err := p.Validate(); err == nil {
		t.Fatal("S3 secret accepted")
	}
}

func FuzzInspect(f *testing.F) {
	b := make([]byte, headerSize+16)
	b[0] = 1
	binary.BigEndian.PutUint32(b[1:5], 3)
	binary.BigEndian.PutUint32(b[5:9], 65536)
	b[9] = 4
	b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
	f.Add(Prefix + base64.RawURLEncoding.EncodeToString(b))
	f.Add(Prefix + "abc")
	f.Add("")
	f.Fuzz(func(t *testing.T, bundle string) {
		if len(bundle) > MaxBundle+1 {
			return
		}
		_, _ = Inspect(bundle)
	})
}

func FuzzNormalizeCode(f *testing.F) {
	f.Add("aar aba abb abd abh abi")
	f.Fuzz(func(t *testing.T, code string) {
		if len(code) > 1024 {
			return
		}
		normalized, err := NormalizeCode(code)
		if err == nil {
			again, e := NormalizeCode(normalized)
			if e != nil || again != normalized {
				t.Fatal("normalization changed")
			}
		}
	})
}

func TestStrictJSONAndPortableTextValidation(t *testing.T) {
	t.Parallel()
	for _, data := range []string{`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `{} {}`, strings.Repeat("[", 66) + strings.Repeat("]", 66)} {
		if uniqueJSON([]byte(data)) == nil {
			t.Fatalf("accepted malformed JSON %s", data)
		}
	}
	if uniqueJSON([]byte(`{"a":[{"b":1},{"b":2}]}`)) != nil {
		t.Fatal("valid nested JSON rejected")
	}
	for _, text := range []string{"../escape", "/absolute", "safe/../escape", "safe\\escape", "safe\x00escape", "safe\u202eescape", string([]byte{0xff})} {
		if RelativePath(text) {
			t.Fatalf("accepted unsafe relative path %q", text)
		}
	}
}

func TestAuthenticatedPayloadLimitsUnknownFieldsAndHeaderConsistency(t *testing.T) {
	p := testPayload()
	code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	bundle, err := Seal(p, code)
	if err != nil {
		t.Fatal(err)
	}
	original, err := decode(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key := argon2.IDKey([]byte(code), original[10:26], 3, 65536, 4, 32)
	defer clear(key)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.PairingID = strings.Repeat("f", 32)
	mismatch, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	unknown := append([]byte(`{"unknown_setting":true,`), data[1:]...)
	duplicate := append([]byte(`{"version":1,`), data[1:]...)
	for _, plain := range [][]byte{bytes.Repeat([]byte("x"), MaxPayload+1), unknown, duplicate, mismatch,
		bytes.Replace(data, []byte(`"require_skill_use":false,`), nil, 1),
		bytes.Replace(data, []byte(`"require_skill_use":false`), []byte(`"require_skill_use":null`), 1),
		bytes.Replace(data, []byte(`"require_skill_use":false`), []byte(`"require_skill_use":true,"Require_Skill_Use":false`), 1),
		bytes.Replace(data, []byte(`"prefix":"archive/"`), []byte(`"Prefix":"archive/"`), 1),
	} {
		var zipped bytes.Buffer
		z := gzip.NewWriter(&zipped)
		if _, err = z.Write(plain); err != nil {
			t.Fatal(err)
		}
		if err = z.Close(); err != nil {
			t.Fatal(err)
		}
		header := append([]byte(nil), original[:headerSize]...)
		b := aead.Seal(header, header[26:50], zipped.Bytes(), header)
		b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
		forged := Prefix + base64.RawURLEncoding.EncodeToString(b)
		if _, err = Open(forged, code, p.CreatedAt); err == nil {
			t.Fatal("accepted authenticated invalid payload")
		}
	}
}

func FuzzPayloadJSON(f *testing.F) {
	data, err := json.Marshal(testPayload())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte(`{"require_skill_use":true,"Require_Skill_Use":false}`))
	f.Add([]byte(`{"inclusions":null}`))
	f.Fuzz(func(t *testing.T, plain []byte) {
		if len(plain) <= MaxPayload {
			_ = payloadJSON(plain)
		}
	})
}
