package systemd

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// platform.Locations.UserUnitDir is what setup's network-filesystem check
// looks at for the units, so it must be where this adapter writes them.
func TestUnitDirIsTheLocationsUserUnitDir(t *testing.T) {
	t.Parallel()
	for _, home := range []string{"/home/me", "/var/lib/svc/home", "/home/with space"} {
		want := Scheduler{}.UnitDir(scheduler.Site{UserHome: home})
		for _, xdg := range []string{"", "/elsewhere"} {
			got := platform.NewLocations(platform.Linux, home, func(key string) string {
				if key == "XDG_CONFIG_HOME" {
					return xdg
				}
				return ""
			}, platform.LocationDeps{}).UserUnitDir
			if got != want {
				t.Errorf("home %s, XDG_CONFIG_HOME %q: Locations.UserUnitDir = %q, the adapter's UnitDir = %q", home, xdg, got, want)
			}
		}
	}
}
