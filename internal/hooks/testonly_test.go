package hooks

// Helpers only tests use, kept out of the production files so deadcode
// (golang.org/x/tools/cmd/deadcode) reports only code that is really dead.

// PlanRemoval prepares the inverse of Plan for uninstall: for each harness,
// a Change whose After is the current file with only hook's installation's
// handlers (and the prototype's) stripped (see Remove). A harness whose hook
// file is missing, or whose file never contained those, yields no Change at
// all, so an unrelated configuration, or one only another installation's
// hooks are in, is never rewritten or reformatted. Apply the result with
// Apply, which keeps its refuse-on-concurrent-edit and rollback behavior.
func PlanRemoval(files Files, hook Hook, harnesses []string) ([]Change, error) {
	changes := []Change{}
	for _, h := range harnesses {
		c, found, err := PlanRemovalOf(files, hook, h)
		if err != nil {
			return nil, err
		}
		if found {
			changes = append(changes, c)
		}
	}
	return changes, nil
}
