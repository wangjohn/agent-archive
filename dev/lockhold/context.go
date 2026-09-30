package main

import (
	"strings"

	"golang.org/x/tools/go/ssa"
)

func set(list string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(list, ",") {
		out[strings.TrimSpace(s)] = true
	}
	return out
}

type edge struct {
	from *ssa.Function
	at   ssa.Instruction
}

// unguarded returns the functions reachable from a root (a function with no
// static caller: main, init, callbacks, interface methods) along calls not
// made inside a critical section of an exempting lock, each with the call
// that first reached it.
func unguarded(mod []*ssa.Function, exemptIDs map[string]bool) map[*ssa.Function]edge {
	guarded := map[ssa.Instruction]bool{}
	for _, fn := range mod {
		eachAcquire(fn, func(call ssa.CallInstruction, info lockInfo, unlock ssa.Value) {
			if returned(fn, unlock) {
				return
			}
			for _, id := range resolveIDs(fn, call.Common(), info, 0) {
				if exemptIDs[id] {
					walkRegion(call, unlock, func(in ssa.Instruction, _ *ssa.CallCommon) { guarded[in] = true })
					return
				}
			}
		})
	}
	type out struct {
		at ssa.Instruction
		to *ssa.Function
	}
	succ := map[*ssa.Function][]out{}
	hasCaller := map[*ssa.Function]bool{}
	inMod := map[*ssa.Function]bool{}
	for _, fn := range mod {
		inMod[fn] = true
	}
	for _, fn := range mod {
		eachCall(fn, func(c *ssa.CallCommon, in ssa.Instruction) {
			for _, g := range callees(c) {
				if inMod[g] {
					succ[fn] = append(succ[fn], out{in, g})
					hasCaller[g] = true
				}
			}
		})
	}
	reach := map[*ssa.Function]edge{}
	var queue []*ssa.Function
	// Test-helper packages (statetest, storagetest, testutil) are not roots;
	// the program's main functions go first so witnesses start there.
	for _, mainFirst := range []bool{true, false} {
		for _, fn := range mod {
			path := fn.Pkg.Pkg.Path()
			if hasCaller[fn] || strings.HasSuffix(path, "test") || strings.Contains(path, "/testutil") || (fn.Pkg.Pkg.Name() == "main") != mainFirst {
				continue
			}
			reach[fn] = edge{}
			queue = append(queue, fn)
		}
	}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		for _, o := range succ[fn] {
			if guarded[o.at] {
				continue
			}
			if _, seen := reach[o.to]; !seen {
				reach[o.to] = edge{fn, o.at}
				queue = append(queue, o.to)
			}
		}
	}
	return reach
}

func witness(reach map[*ssa.Function]edge, fn *ssa.Function) string {
	var chain []string
	for i := 0; fn != nil && i < 12; i++ {
		chain = append([]string{name(fn)}, chain...)
		fn = reach[fn].from
	}
	return strings.Join(chain, " → ")
}
