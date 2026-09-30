// Package trace records where one command's time goes, for
// AGENT_ARCHIVE_TRACE. It is process-wide: a command runs once per process,
// and a global recorder lets the storage, reader, state and collector
// packages record their spans without every caller threading a context
// through, so it needs no changes to the commands built on them.
//
// Spans hold only fixed names, durations and counts: never keys, titles,
// paths or other archive content, so a trace is safe to paste into an
// issue. With tracing off every function here is a nil check.
package trace

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Span is one timed piece of work. A nil *Span is valid and records
// nothing, which is what every function returns while tracing is off.
type Span struct {
	rec    *recorder
	name   string
	start  time.Time
	end    time.Time
	counts map[string]int64
	order  []string // count names, first-recorded first
	// children are spans made with Child: their parent is explicit, not
	// inferred from time, since siblings made that way usually overlap.
	children []*Span
}

type recorder struct {
	mu    sync.Mutex
	start time.Time
	top   []*Span // spans made with Start, nested by time when written
}

var active atomic.Pointer[recorder]

// Enable starts recording, replacing any earlier recording, and returns a
// function that stops it.
func Enable() (disable func()) {
	rec := &recorder{start: time.Now()}
	active.Store(rec)
	return func() { active.CompareAndSwap(rec, nil) }
}

// Enabled reports whether spans are being recorded.
func Enabled() bool { return active.Load() != nil }

// Start begins a span whose place in the tree is decided when the trace is
// written: under the innermost span made with Start that began before it and
// ended after it. Use it at a package's entry points, where the caller's span
// is not at hand.
func Start(name string) *Span {
	rec := active.Load()
	if rec == nil {
		return nil
	}
	span := &Span{rec: rec, name: name, start: time.Now()}
	rec.mu.Lock()
	rec.top = append(rec.top, span)
	rec.mu.Unlock()
	return span
}

// Child begins a span under s. Use it for work that runs concurrently with
// its siblings, which time alone cannot place.
func (s *Span) Child(name string) *Span {
	if s == nil {
		return nil
	}
	child := &Span{rec: s.rec, name: name, start: time.Now()}
	s.rec.mu.Lock()
	s.children = append(s.children, child)
	s.rec.mu.Unlock()
	return child
}

// Count adds n to the span's count called name.
func (s *Span) Count(name string, n int) {
	if s == nil {
		return
	}
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if s.counts == nil {
		s.counts = map[string]int64{}
	}
	if _, seen := s.counts[name]; !seen {
		s.order = append(s.order, name)
	}
	s.counts[name] += int64(n)
}

// End ends the span. Ending it again changes nothing.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if s.end.IsZero() {
		s.end = time.Now()
	}
}

// Write renders the recording as an indented tree: each span's start offset
// from Enable, its duration, and its counts. Sibling spans with the same name
// are folded into one line with how many there were, their total and their
// longest duration. A span still running is shown up to now.
func Write(w io.Writer) {
	rec := active.Load()
	if rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	now := time.Now()
	roots := nest(rec.top, now)
	var b strings.Builder
	fmt.Fprintf(&b, "agent-archive trace (start offset, duration):\n")
	writeLevel(&b, rec.start, roots, 0, now)
	_, _ = io.WriteString(w, b.String())
}

type node struct {
	span     *Span
	children []*node
}

func endOf(s *Span, now time.Time) time.Time {
	if s.end.IsZero() {
		return now
	}
	return s.end
}

// nest places each span made with Start under the innermost earlier span
// that contains it in time, then adds the explicit children of every span.
func nest(top []*Span, now time.Time) []*node {
	spans := append([]*Span(nil), top...)
	// Earlier first; of two starting together, the longer one contains the
	// other and so comes first.
	sort.SliceStable(spans, func(i, j int) bool {
		if !spans[i].start.Equal(spans[j].start) {
			return spans[i].start.Before(spans[j].start)
		}
		return endOf(spans[i], now).After(endOf(spans[j], now))
	})
	var roots []*node
	var open []*node // the chain of containing spans, outermost first
	for _, span := range spans {
		n := explicit(span)
		for len(open) > 0 {
			parent := open[len(open)-1].span
			if !span.start.Before(parent.start) && !endOf(span, now).After(endOf(parent, now)) {
				break
			}
			open = open[:len(open)-1]
		}
		if len(open) == 0 {
			roots = append(roots, n)
		} else {
			parent := open[len(open)-1]
			parent.children = append(parent.children, n)
		}
		open = append(open, n)
	}
	return roots
}

func explicit(span *Span) *node {
	n := &node{span: span}
	for _, child := range span.children {
		n.children = append(n.children, explicit(child))
	}
	return n
}

// writeLevel writes siblings in start order, folding those that share a
// name, and recurses into the children of each group.
func writeLevel(b *strings.Builder, origin time.Time, nodes []*node, depth int, now time.Time) {
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].span.start.Before(nodes[j].span.start) })
	var names []string
	groups := map[string][]*node{}
	for _, n := range nodes {
		if _, seen := groups[n.span.name]; !seen {
			names = append(names, n.span.name)
		}
		groups[n.span.name] = append(groups[n.span.name], n)
	}
	for _, name := range names {
		group := groups[name]
		first := group[0].span
		var total, longest time.Duration
		counts := map[string]int64{}
		var order []string
		var children []*node
		for _, n := range group {
			d := endOf(n.span, now).Sub(n.span.start)
			total += d
			longest = max(longest, d)
			for _, key := range n.span.order {
				if _, seen := counts[key]; !seen {
					order = append(order, key)
				}
				counts[key] += n.span.counts[key]
			}
			children = append(children, n.children...)
		}
		label := name
		timing := millis(total)
		if len(group) > 1 {
			label = fmt.Sprintf("%s ×%d", name, len(group))
			timing = fmt.Sprintf("%s (longest %s)", millis(total), millis(longest))
		}
		var parts []string
		for _, key := range order {
			parts = append(parts, fmt.Sprintf("%s %d", key, counts[key]))
		}
		detail := ""
		if len(parts) > 0 {
			detail = "  · " + strings.Join(parts, ", ")
		}
		fmt.Fprintf(b, "%9s  %s%s  %s%s\n", "+"+millis(first.start.Sub(origin)), strings.Repeat("  ", depth), label, timing, detail)
		writeLevel(b, origin, children, depth+1, now)
	}
}

func millis(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}
