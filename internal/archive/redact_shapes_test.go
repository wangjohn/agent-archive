package archive

import (
	"regexp"
	"strings"
	"testing"
)

// Every token shape is only searched on lines holding one of its needles
// (see linePattern). One sample per credentialShapeTable entry, in order,
// proves each entry's needles find what the entry matches; adding an entry
// without a sample fails here. The samples are split so secret scanners do
// not take this file for a leak.
func TestCredentialShapeNeedles(t *testing.T) {
	t.Parallel()
	a := func(n int) string { return strings.Repeat("a", n) }
	d := func(n int) string { return strings.Repeat("0", n) }
	samples := []string{
		"eyJ" + a(10) + "." + a(10) + "." + a(10),
		"gh" + "p_" + a(24),
		"github" + "_pat_" + a(24),
		"xo" + "xb-" + a(12),
		"xa" + "pp-1-" + a(12),
		"AK" + "IA" + strings.Repeat("A", 16),
		"sk" + "-" + a(16),
		"sk" + "_live_" + a(12),
		"wh" + "sec_" + a(24),
		"gl" + "pat-" + a(24),
		"AI" + "za" + a(32),
		"GO" + "CSPX-" + a(24),
		"ya" + "29." + a(24),
		"hf" + "_" + a(32),
		"api" + "_org_" + a(32),
		"np" + "m_" + a(36),
		"py" + "pi-AgE" + a(52),
		"SG" + "." + a(18) + "." + a(18),
		"shp" + "at_" + d(32),
		"do" + "p_v1_" + d(64),
		"hv" + "s." + a(26),
		"da" + "pi" + d(32),
		"lin" + "_api_" + a(32),
		"gl" + "sa_" + a(32),
		"gl" + "c_" + a(32),
		"PM" + "AK-" + d(24) + "-" + d(34),
		"NR" + "AK-" + strings.Repeat("A", 27),
		"sn" + "try_" + a(32),
		"AT" + "ATT3" + a(32),
		"fi" + "gd_" + a(32),
		"dp" + ".pt." + a(32),
		"AGE-SECRET" + "-KEY-1" + strings.Repeat("Q", 44),
		"ke" + "y-" + d(32),
		d(9) + ":AA" + a(33),
		"abc" + "8Q~" + a(32),
		"LS0tLS1" + "CRUdJTi" + a(24),
	}
	if len(samples) != len(credentialShapeTable) {
		t.Fatalf("%d samples for %d shapes: add a sample for each new shape", len(samples), len(credentialShapeTable))
	}
	for i, shape := range credentialShapeTable {
		sample := "before " + samples[i] + " after"
		if !regexp.MustCompile(shape.pattern).MatchString(sample) {
			t.Errorf("shape %d (%s) does not match its sample %q", i, shape.pattern, samples[i])
			continue
		}
		gate := linePattern{re: regexp.MustCompile(shape.pattern), needles: shape.needles}
		if len(lineMatches(gate, newNeedleText("x\n"+sample+"\ny"))) == 0 {
			t.Errorf("shape %d (%s): its needles %q miss %q", i, shape.pattern, shape.needles, samples[i])
		}
		if out, _ := redactSensitive(sample); strings.Contains(out, samples[i]) {
			t.Errorf("shape %d: %q survived: %q", i, samples[i], out)
		}
	}
}
