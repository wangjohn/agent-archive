package capture

import "time"

func queueAdmissionIntent(home, harness string, kind hookEventKind, payload map[string]any, now time.Time) (bool, error) {
	return queueAdmissionIntentAfterStage(home, harness, kind, payload, now, nil)
}

// afterStage runs after the durable file stage but before the commit lock,
// allowing tests to change configuration in that admission window.
func queueAdmissionIntentAfterStage(home, harness string, kind hookEventKind, payload map[string]any, now time.Time, afterStage func()) (bool, error) {
	return queueAdmissionIntentWithGeneration(home, harness, kind, payload, now, afterStage, nil)
}
