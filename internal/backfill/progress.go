package backfill

import "sync"

// PlanningProgress describes one planning phase. Total is zero when discovery
// has not established a work count. Counts describe this phase, not the import.
type PlanningProgress struct {
	Stage string
	Done  int
	Total int
}

func (e Environment) planningProgress(stage string, done, total int) {
	if e.Progress != nil {
		e.Progress(PlanningProgress{Stage: stage, Done: done, Total: total})
	}
}

// planningCounter serializes worker notifications and counts completed work.
func (e Environment) planningCounter(stage string, total int) func() {
	if e.Progress == nil {
		return func() {}
	}
	e.planningProgress(stage, 0, total)
	var mu sync.Mutex
	done := 0
	return func() {
		mu.Lock()
		defer mu.Unlock()
		done++
		e.planningProgress(stage, done, total)
	}
}
