//go:build !race

package rolloutcatalog

// raceEnabled leaves the plain scale test on the production default sweep bound.
const raceEnabled = false
