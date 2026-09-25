package setupjournal

import "errors"

// fakeLaunchd stands in for launchd: each call goes to the matching func.
// With none set, a job is missing, and loading or stopping one fails.
type fakeLaunchd struct {
	state  func(plist string) string
	load   func(plist string) error
	unload func(plist string) error
}

func (f fakeLaunchd) JobState(plist string) string {
	if f.state == nil {
		return "missing"
	}
	return f.state(plist)
}

func (f fakeLaunchd) Load(plist string) error {
	if f.load == nil {
		return errors.New("fake launchd: unexpected load of " + plist)
	}
	return f.load(plist)
}

func (f fakeLaunchd) Unload(plist string) error {
	if f.unload == nil {
		return errors.New("fake launchd: unexpected unload of " + plist)
	}
	return f.unload(plist)
}

// noLock stands in for the collector lock Recover takes.
func noLock() (func(), error) { return func() {}, nil }
