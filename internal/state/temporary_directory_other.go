//go:build !darwin && !linux

package state

import (
	"os"
	"time"
)

func temporaryQuotaLock(*os.Root, time.Duration) (func(), error) {
	return nil, ErrAdmissionStageRecovery
}
