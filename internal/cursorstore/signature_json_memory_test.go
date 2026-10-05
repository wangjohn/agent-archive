//go:build !race

package cursorstore

import (
	"bufio"
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestStreamedSignatureDiscardsLargeScalars(t *testing.T) {
	const size = 3 << 20
	for _, timestamp := range []bool{false, true} {
		value := `"` + strings.Repeat("x", size) + `"`
		if timestamp {
			value = "0." + strings.Repeat("0", size) + "1e" + strconv.Itoa(size+1)
		}
		p := signatureJSON{r: bufio.NewReader(strings.NewReader(value))}
		first, err := p.r.ReadByte()
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		var scalar []byte
		if timestamp {
			scalar, err = p.scalar(first, true)
		} else {
			err = p.skip(first, 1)
		}
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<10 {
			t.Fatalf("timestamp=%v: streamed scalar allocated %d bytes", timestamp, allocated)
		}
		if timestamp {
			var number float64
			if err := json.Unmarshal(scalar, &number); err != nil || number != 1 {
				t.Fatalf("large zero-run timestamp: %g %v", number, err)
			}
		}
	}
}
