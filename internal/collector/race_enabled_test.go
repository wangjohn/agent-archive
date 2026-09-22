//go:build race

package collector

// raceEnabled lets a performance test scale itself down under the race
// detector, which slows filtering and compression by an order of magnitude
// without changing what the test proves.
const raceEnabled = true
