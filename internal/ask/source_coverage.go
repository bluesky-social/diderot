package ask

import (
	"context"
	"sync"
)

// sourceCoverage records which sources a turn's searches have covered, so
// that each retrieve result can say which have not been searched at all.
//
// Whether a source is searched must not rest on the model walking a list of
// sources to its end. A turn that searched six sources of seven, one call
// each, answered "not found" while the seventh held the answer; which source
// went unsearched followed the order the sources were registered in. A
// retrieve with no source filter covers every source at once, and after one
// the list is empty.
type sourceCoverage struct {
	mu      sync.Mutex
	all     bool
	covered map[string]struct{}
}

type sourceCoverageKey struct{}

// withSourceCoverage starts a turn's record of the sources it has searched.
func withSourceCoverage(ctx context.Context) context.Context {
	return context.WithValue(ctx, sourceCoverageKey{}, &sourceCoverage{covered: map[string]struct{}{}})
}

func sourceCoverageFromContext(ctx context.Context) *sourceCoverage {
	coverage, _ := ctx.Value(sourceCoverageKey{}).(*sourceCoverage)
	return coverage
}

// searched records one completed search and returns, of the sources given,
// those no search of the turn has covered yet, in the order given. A search
// with no filter covers every source. A nil receiver (a retrieve outside an
// Ask turn) records nothing and returns nothing.
func (c *sourceCoverage) searched(filter, all []string) []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(filter) == 0 {
		c.all = true
	}
	for _, name := range filter {
		c.covered[name] = struct{}{}
	}
	if c.all {
		return nil
	}
	var missing []string
	for _, name := range all {
		if _, ok := c.covered[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}
