// Command lockhold is a prototype whole-program check for agent-archive. It
// finds the critical sections of the locks hooks wait on with a short
// deadline (local.NamedLock and NamedLockWait, and the wrappers that return
// their unlock func) and reports slow work reachable inside them through
// static calls: an fsync, a nested NamedLockWait, or a sleep.
//
// A critical section is reported only if some caller path from a program
// root reaches it without holding an exempting lock: hooks take hooks.lock
// before the request lock, so no hook can be waiting behind a holder of
// both. "//lockhold:ignore <reason>" on or above a line silences it.
//
// Run it from the repository root:
//
//	(cd dev/lockhold && go run . -C ../..)
//
// It exits 1 when it reports anything, and 2 when a lock named in -hotlist
// or -exempt is no longer taken anywhere (it was renamed).
package main

import (
	"flag"
	"fmt"
	"go/constant"
	"go/token"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

var (
	dir       = flag.String("C", ".", "module directory")
	module    = flag.String("module", "github.com/wangjohn/agent-archive/", "module path prefix to analyze")
	localPkg  = flag.String("local", "github.com/wangjohn/agent-archive/internal/local", "package with NamedLock/NamedLockWait")
	roots     = flag.String("roots", "github.com/wangjohn/agent-archive/internal/capture", "comma list of latency-sensitive packages (hook runtime)")
	policy    = flag.String("hot", "list", "which locks are hot: list (-hotlist) | hook (waited on from -roots) | waited | all")
	showChain = flag.Bool("chain", true, "print the call chain to the slow operation")
	hotList   = flag.String("hotlist", "state.requestLockName(),state.subagentLockName()", "with -hot=list: comma list of hot lock identities (a constant name, or the function that makes it)")
	exempt    = flag.String("exempt", `"hooks.lock"`, "comma list of lock identities no hook can wait behind; a critical section reached only under one is skipped (empty: none)")
)

// lockInfo describes a function that acquires a lock and returns its unlock
// func as result 0.
type lockInfo struct {
	param int    // index of the parameter naming the lock, or -1
	id    string // lock identity when param < 0
	wait  bool   // waits (NamedLockWait) rather than failing fast
}

type slowInfo struct {
	kind  string
	chain []string
}

type finding struct {
	holder  *ssa.Function
	acqPos  token.Pos
	lockIDs []string
	callPos token.Pos
	slow    slowInfo
	callee  string
	escaped bool
}

var (
	prog      *ssa.Program
	acquirers = map[*ssa.Function]lockInfo{}
	slowMemo  = map[*ssa.Function]*slowInfo{}
	visiting  = map[*ssa.Function]bool{}
	callers   = map[*ssa.Function][]*ssa.CallCommon{}
)

func main() {
	flag.Parse()
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: *dir, Tests: false}
	initial, err := packages.Load(cfg, "./...")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if packages.PrintErrors(initial) > 0 {
		os.Exit(2)
	}
	prog, _ = ssautil.AllPackages(initial, ssa.InstantiateGenerics)
	prog.Build()

	var mod []*ssa.Function
	for fn := range ssautil.AllFunctions(prog) {
		if fn.Pkg != nil && strings.HasPrefix(fn.Pkg.Pkg.Path(), *module) && fn.Blocks != nil {
			mod = append(mod, fn)
		}
	}
	sort.Slice(mod, func(i, j int) bool { return mod[i].Pos() < mod[j].Pos() })

	for fn := range ssautil.AllFunctions(prog) {
		if fn.Pkg == nil || fn.Pkg.Pkg.Path() != *localPkg {
			continue
		}
		switch fn.Name() {
		case "NamedLock":
			acquirers[fn] = lockInfo{param: 1}
		case "NamedLockWait":
			acquirers[fn] = lockInfo{param: 1, wait: true}
		}
	}
	for _, fn := range mod {
		eachCall(fn, func(c *ssa.CallCommon, _ ssa.Instruction) {
			for _, g := range callees(c) {
				callers[g] = append(callers[g], c)
			}
		})
	}
	// Wrappers: functions that return an acquirer's unlock func.
	for changed := true; changed; {
		changed = false
		for _, fn := range mod {
			if _, ok := acquirers[fn]; ok {
				continue
			}
			eachAcquire(fn, func(call ssa.CallInstruction, info lockInfo, unlock ssa.Value) {
				if !returned(fn, unlock) {
					return
				}
				w := lockInfo{param: -1, wait: info.wait}
				id := argID(fn, call.Common(), info)
				if p, ok := strings.CutPrefix(id, "param#"); ok {
					fmt.Sscan(p, &w.param)
				} else {
					w.id = id
				}
				acquirers[fn] = w
				changed = true
			})
		}
	}

	hot := hotLocks(mod)
	if *policy == "list" {
		hot = set(*hotList)
		missing := unknownIDs(mod, hot)
		if *exempt != "" {
			missing = append(missing, unknownIDs(mod, set(*exempt))...)
		}
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "lockhold: no lock is taken as %s; update -hotlist/-exempt after a rename\n", strings.Join(missing, ", "))
			os.Exit(2)
		}
	}
	fmt.Printf("# policy=%s hot locks: %s\n", *policy, strings.Join(keys(hot), ", "))
	var reach map[*ssa.Function]edge
	if *exempt != "" {
		reach = unguarded(mod, set(*exempt))
		fmt.Printf("# exempt when reached only under: %s\n", *exempt)
	}

	var findings []finding
	regions, escapes, exempted := 0, 0, 0
	for _, fn := range mod {
		eachAcquire(fn, func(call ssa.CallInstruction, info lockInfo, unlock ssa.Value) {
			if returned(fn, unlock) {
				return
			}
			ids := resolveIDs(fn, call.Common(), info, 0)
			if !anyHot(ids, hot) {
				return
			}
			if reach != nil {
				if _, ok := reach[fn]; !ok {
					exempted++
					return
				}
			}
			regions++
			esc := escapesTo(unlock)
			if esc {
				escapes++
			}
			seen := map[token.Pos]bool{}
			walkRegion(call, unlock, func(in ssa.Instruction, c *ssa.CallCommon) {
				for _, g := range callees(c) {
					var s *slowInfo
					if info, ok := acquirers[g]; ok && info.wait {
						s = &slowInfo{kind: "lock-wait", chain: []string{name(g)}}
					} else {
						s = slow(g)
					}
					if s == nil || seen[in.Pos()] || ignored(in.Pos()) {
						continue
					}
					seen[in.Pos()] = true
					findings = append(findings, finding{holder: fn, acqPos: call.Pos(), lockIDs: ids, callPos: in.Pos(), slow: *s, callee: name(g), escaped: esc})
				}
			})
		})
	}
	fmt.Printf("# %d hot critical sections (%d where the unlock func escapes; %d more reached only under an exempting lock), %d findings\n\n", regions, escapes, exempted, len(findings))
	sort.Slice(findings, func(i, j int) bool {
		a, b := prog.Fset.Position(findings[i].callPos), prog.Fset.Position(findings[j].callPos)
		return a.Filename < b.Filename || a.Filename == b.Filename && a.Line < b.Line
	})
	for _, f := range findings {
		p := prog.Fset.Position(f.callPos)
		a := prog.Fset.Position(f.acqPos)
		fmt.Printf("%s:%d: %s under %s (acquired line %d in %s)%s\n", rel(p.Filename), p.Line, f.slow.kind, strings.Join(f.lockIDs, "|"), a.Line, name(f.holder), map[bool]string{true: " [unlock escapes]"}[f.escaped])
		if *showChain {
			fmt.Printf("    %s\n", strings.Join(append([]string{f.callee}, f.slow.chain...), " → "))
			if reach != nil {
				fmt.Printf("    reached without the exempting lock from: %s\n", witness(reach, f.holder))
			}
		}
	}
	if len(findings) > 0 {
		os.Exit(1)
	}
}

// ignored reports a "//lockhold:ignore <reason>" comment on the line of pos
// or the line above it.
func ignored(pos token.Pos) bool {
	p := prog.Fset.Position(pos)
	b, err := os.ReadFile(p.Filename)
	if err != nil {
		return false
	}
	lines := strings.Split(string(b), "\n")
	for _, n := range []int{p.Line - 1, p.Line - 2} {
		if n >= 0 && n < len(lines) && strings.Contains(lines[n], "//lockhold:ignore ") {
			return true
		}
	}
	return false
}

// unknownIDs are the identities in want that no acquire in the program takes.
func unknownIDs(mod []*ssa.Function, want map[string]bool) []string {
	seen := map[string]bool{}
	for _, fn := range mod {
		eachAcquire(fn, func(call ssa.CallInstruction, info lockInfo, _ ssa.Value) {
			for _, id := range resolveIDs(fn, call.Common(), info, 0) {
				seen[id] = true
			}
		})
	}
	var out []string
	for id := range want {
		if !seen[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func rel(path string) string {
	if i := strings.Index(path, "/internal/"); i >= 0 {
		return path[i+1:]
	}
	if i := strings.Index(path, "/cmd/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func name(fn *ssa.Function) string {
	s := fn.String()
	s = strings.ReplaceAll(s, *module+"internal/", "")
	return s
}

// eachCall visits the calls and defers in fn (not go statements).
func eachCall(fn *ssa.Function, visit func(*ssa.CallCommon, ssa.Instruction)) {
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			switch in := in.(type) {
			case *ssa.Call:
				visit(in.Common(), in)
			case *ssa.Defer:
				visit(in.Common(), in)
			}
		}
	}
}

// callees resolves a call's possible targets: a static callee, a closure, or
// the closures a phi merges. Interface and parameter calls are not resolved.
func callees(c *ssa.CallCommon) []*ssa.Function {
	if c.IsInvoke() {
		return nil
	}
	var out []*ssa.Function
	var visit func(v ssa.Value, depth int)
	visit = func(v ssa.Value, depth int) {
		if depth > 4 {
			return
		}
		switch v := v.(type) {
		case *ssa.Function:
			out = append(out, v)
		case *ssa.MakeClosure:
			out = append(out, v.Fn.(*ssa.Function))
		case *ssa.Phi:
			for _, e := range v.Edges {
				visit(e, depth+1)
			}
		case *ssa.ChangeType:
			visit(v.X, depth+1)
		}
	}
	visit(c.Value, 0)
	return out
}

func eachAcquire(fn *ssa.Function, visit func(ssa.CallInstruction, lockInfo, ssa.Value)) {
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			call, ok := in.(*ssa.Call)
			if !ok {
				continue
			}
			for _, g := range callees(call.Common()) {
				info, ok := acquirers[g]
				if !ok {
					continue
				}
				unlock := extract0(call)
				if unlock == nil {
					continue
				}
				visit(call, info, unlock)
				break
			}
		}
	}
}

func extract0(call *ssa.Call) ssa.Value {
	for _, r := range *call.Referrers() {
		if e, ok := r.(*ssa.Extract); ok && e.Index == 0 {
			return e
		}
	}
	return nil
}

func returned(fn *ssa.Function, v ssa.Value) bool {
	for _, r := range *v.Referrers() {
		if ret, ok := r.(*ssa.Return); ok && len(ret.Results) > 0 && ret.Results[0] == v {
			return true
		}
	}
	return false
}

// escapesTo reports an unlock func used other than by calling or deferring
// it directly, or capturing it in a closure.
func escapesTo(v ssa.Value) bool {
	for _, r := range *v.Referrers() {
		switch r := r.(type) {
		case *ssa.Call:
			if r.Call.Value != v {
				return true
			}
		case *ssa.Defer:
			if r.Call.Value != v {
				return true
			}
		case *ssa.MakeClosure, *ssa.DebugRef:
		default:
			return true
		}
	}
	return false
}

// argID names the lock an acquire call takes.
func argID(fn *ssa.Function, c *ssa.CallCommon, info lockInfo) string {
	if info.param < 0 {
		return info.id
	}
	args := c.Args
	if info.param >= len(args) {
		return "?"
	}
	return valueID(fn, args[info.param])
}

func valueID(fn *ssa.Function, v ssa.Value) string {
	switch v := v.(type) {
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.String {
			return fmt.Sprintf("%q", constant.StringVal(v.Value))
		}
	case *ssa.Call:
		if g := v.Call.StaticCallee(); g != nil {
			return name(g) + "()"
		}
	case *ssa.Parameter:
		for i, p := range fn.Params {
			if p == v {
				return fmt.Sprintf("param#%d", i)
			}
		}
	case *ssa.FreeVar:
		return "freevar:" + v.Name()
	}
	return "dynamic:" + v.String()
}

// resolveIDs follows a lock name that is a parameter of fn to fn's callers.
func resolveIDs(fn *ssa.Function, c *ssa.CallCommon, info lockInfo, depth int) []string {
	id := argID(fn, c, info)
	p, ok := strings.CutPrefix(id, "param#")
	if !ok || depth > 3 {
		return []string{id}
	}
	var idx int
	fmt.Sscan(p, &idx)
	seen := map[string]bool{}
	for _, cc := range callers[fn] {
		off := 0
		if fn.Signature.Recv() != nil {
			off = 0 // receiver is Params[0] and Args[0] for static method calls
		}
		if idx+off >= len(cc.Args) {
			continue
		}
		seen[valueID(fn, cc.Args[idx+off])] = true
	}
	if len(seen) == 0 {
		return []string{id + " of " + name(fn)}
	}
	return keys(seen)
}

func anyHot(ids []string, hot map[string]bool) bool {
	if *policy == "all" {
		return true
	}
	for _, id := range ids {
		if hot[id] {
			return true
		}
	}
	return false
}

// hotLocks are the locks some latency-sensitive caller waits on.
func hotLocks(mod []*ssa.Function) map[string]bool {
	hot := map[string]bool{}
	var reach map[*ssa.Function]bool
	if *policy == "hook" {
		reach = map[*ssa.Function]bool{}
		var visit func(*ssa.Function)
		visit = func(fn *ssa.Function) {
			if reach[fn] || fn.Blocks == nil || fn.Pkg == nil || !strings.HasPrefix(fn.Pkg.Pkg.Path(), *module) {
				return
			}
			reach[fn] = true
			for _, anon := range fn.AnonFuncs {
				visit(anon)
			}
			eachCall(fn, func(c *ssa.CallCommon, _ ssa.Instruction) {
				for _, g := range callees(c) {
					visit(g)
				}
			})
		}
		for _, fn := range mod {
			for _, r := range strings.Split(*roots, ",") {
				if fn.Pkg.Pkg.Path() == r {
					visit(fn)
				}
			}
		}
	}
	for _, fn := range mod {
		if reach != nil && !reach[fn] {
			continue
		}
		eachAcquire(fn, func(call ssa.CallInstruction, info lockInfo, unlock ssa.Value) {
			if !info.wait {
				return
			}
			for _, id := range resolveIDs(fn, call.Common(), info, 0) {
				if !strings.HasPrefix(id, "param#") {
					hot[id] = true
				}
			}
		})
	}
	return hot
}

// walkRegion visits the calls reachable from acquire before a call of
// unlock (or a closure that captures it). A deferred unlock does not end the
// region, so it runs to the function's returns.
func walkRegion(acquire ssa.CallInstruction, unlock ssa.Value, visit func(ssa.Instruction, *ssa.CallCommon)) {
	isUnlock := func(c *ssa.CallCommon) bool {
		if c.Value == unlock {
			return true
		}
		if mc, ok := c.Value.(*ssa.MakeClosure); ok {
			for _, b := range mc.Bindings {
				if b == unlock {
					return true
				}
			}
		}
		return false
	}
	start := acquire.Block()
	idx := 0
	for i, in := range start.Instrs {
		if in == acquire {
			idx = i + 1
		}
	}
	visited := map[*ssa.BasicBlock]bool{}
	var walk func(b *ssa.BasicBlock, from int)
	walk = func(b *ssa.BasicBlock, from int) {
		for _, in := range b.Instrs[from:] {
			switch in := in.(type) {
			case *ssa.Call:
				if isUnlock(in.Common()) {
					return
				}
				visit(in, in.Common())
			case *ssa.Defer:
				if !isUnlock(in.Common()) {
					visit(in, in.Common())
				}
			}
		}
		succs := b.Succs
		if next := okBranch(b, acquire); next != nil {
			succs = []*ssa.BasicBlock{next}
		}
		for _, s := range succs {
			if !visited[s] {
				visited[s] = true
				walk(s, 0)
			}
		}
	}
	walk(start, idx)
}

// slow reports whether fn may, through static calls, fsync, wait for a
// lock, or sleep.
func slow(fn *ssa.Function) *slowInfo {
	if s, ok := slowMemo[fn]; ok {
		return s
	}
	if seed := seedKind(fn); seed != "" {
		s := &slowInfo{kind: seed}
		slowMemo[fn] = s
		return s
	}
	if info, ok := acquirers[fn]; ok && info.wait && fn.Pkg != nil && fn.Pkg.Pkg.Path() == *localPkg {
		s := &slowInfo{kind: "lock-wait"}
		slowMemo[fn] = s
		return s
	}
	if visiting[fn] || fn.Blocks == nil {
		return nil
	}
	visiting[fn] = true
	defer delete(visiting, fn)
	var found *slowInfo
	check := func(g *ssa.Function) {
		if found != nil {
			return
		}
		if s := slow(g); s != nil {
			found = &slowInfo{kind: s.kind, chain: append([]string{name(g)}, s.chain...)}
		}
	}
	eachCall(fn, func(c *ssa.CallCommon, _ ssa.Instruction) {
		for _, g := range callees(c) {
			check(g)
		}
	})
	// A closure fn makes may be run by whatever fn passes it to.
	for _, anon := range fn.AnonFuncs {
		check(anon)
	}
	slowMemo[fn] = found
	return found
}

func seedKind(fn *ssa.Function) string {
	if fn.Pkg == nil {
		// Methods such as (*os.File).Sync have Pkg set; synthetic wrappers may not.
		if fn.Signature.Recv() == nil {
			return ""
		}
	}
	s := fn.String()
	switch s {
	case "(*os.File).Sync", "syscall.Fsync", "golang.org/x/sys/unix.Fsync", "syscall.Fdatasync", "golang.org/x/sys/unix.Fdatasync":
		return "fsync"
	case "time.Sleep":
		return "sleep"
	}
	return ""
}

// okBranch is the successor of b taken when the acquire succeeded, if b ends
// by testing the acquire's error against nil; the lock is not held on the
// other branch.
func okBranch(b *ssa.BasicBlock, acquire ssa.CallInstruction) *ssa.BasicBlock {
	ifInstr, ok := b.Instrs[len(b.Instrs)-1].(*ssa.If)
	if !ok {
		return nil
	}
	cmp, ok := ifInstr.Cond.(*ssa.BinOp)
	if !ok || (cmp.Op != token.NEQ && cmp.Op != token.EQL) {
		return nil
	}
	isErr := func(v ssa.Value) bool {
		e, ok := v.(*ssa.Extract)
		return ok && e.Tuple == acquire.Value() && e.Index == 1
	}
	isNil := func(v ssa.Value) bool {
		c, ok := v.(*ssa.Const)
		return ok && c.IsNil()
	}
	if !(isErr(cmp.X) && isNil(cmp.Y) || isErr(cmp.Y) && isNil(cmp.X)) {
		return nil
	}
	if cmp.Op == token.NEQ {
		return b.Succs[1]
	}
	return b.Succs[0]
}
