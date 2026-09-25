package setupjournal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// launchJobActive reports whether a launchd job state from Env.JobState means
// the job is loaded, whether or not it is running at the moment.
func launchJobActive(job string) bool {
	//lint:ignore LV1001 Env.JobState (cli.go) reports launchd states as plain strings, and tests stub it with string-returning funcs
	return job == "loaded" || job == "running"
}

// jobAnotherInstallation is the job state of a label launchd has loaded
// from a plist other than the one asked about: the job belongs to another
// installation (the user's real one, seen from a sandboxed HOME, say), and
// nothing here may stop or replace it.
const jobAnotherInstallation = "another_installation"
