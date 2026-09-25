//go:build race

package archive

// raceEnabled lets a timing test skip under the race detector, which slows
// redaction by an order of magnitude and makes its timings meaningless.
const raceEnabled = true
