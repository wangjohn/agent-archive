package pairing

import (
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

//go:embed eff_short_wordlist_2_0.txt
var wordlist string

func words() []string {
	fields := strings.Fields(wordlist)
	out := make([]string, 0, len(fields)/2)
	for i := 1; i < len(fields); i += 2 {
		out = append(out, fields[i])
	}
	return out
}

// NewCode selects six independent words with unbiased cryptographic sampling.
func NewCode() (string, error) {
	list := words()
	chosen := make([]string, 6)
	for i := range chosen {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
		if err != nil {
			return "", err
		}
		chosen[i] = list[n.Int64()]
	}
	return strings.Join(chosen, "-"), nil
}

// NormalizeCode accepts full words or unique three-letter prefixes, with
// case-insensitive spaces or hyphens. Invalid prefixes fail before derivation.
func NormalizeCode(code string) (string, error) {
	if len(code) > 512 {
		return "", errors.New("pairing code is too long")
	}
	parts := strings.FieldsFunc(strings.ToLower(code), func(r rune) bool { return r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r' })
	if len(parts) != 6 {
		return "", errors.New("enter the six pairing words or their three-letter prefixes")
	}
	list := words()
	for i, part := range parts {
		found := ""
		for _, word := range list {
			if word == part || (len(part) == 3 && strings.HasPrefix(word, part)) {
				found = word
				break
			}
		}
		if found == "" {
			hint := ""
			if len(part) == 3 {
				for _, word := range list {
					prefix := word[:3]
					diff := 0
					for j := range 3 {
						if part[j] != prefix[j] {
							diff++
						}
					}
					if diff == 1 {
						hint = prefix
						break
					}
				}
			}
			if hint != "" {
				return "", fmt.Errorf("word %d has an unknown prefix; did you mean %s?", i+1, hint)
			}
			return "", fmt.Errorf("word %d is unknown; check it on the source machine", i+1)
		}
		parts[i] = found
	}
	return strings.Join(parts, "-"), nil
}
