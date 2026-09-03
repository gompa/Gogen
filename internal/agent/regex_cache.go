package agent

import (
	"regexp"
	"sync"
)

// regexMemoMax bounds the number of distinct patterns held in the compiled
// regex memo.
const regexMemoMax = 256

var (
	// regexMemo caches compiled regexes keyed by pattern string so repeated
	// compiles of the same pattern (e.g. the agent re-running the same
	// search across turns) skip regexp.Compile. Eviction is FIFO by
	// insertion order.
	regexMemo = make(map[string]*regexp.Regexp)
	// regexMemoOrder tracks insertion order for FIFO eviction.
	regexMemoOrder []string
	regexMemoMu    sync.Mutex
)

// compiledRegex returns the compiled form of pattern, memoizing successful
// compiles so repeated calls with the same pattern are cheap. Compile
// errors are not cached and are returned to the caller.
func compiledRegex(pattern string) (*regexp.Regexp, error) {
	regexMemoMu.Lock()
	if re, ok := regexMemo[pattern]; ok {
		regexMemoMu.Unlock()
		return re, nil
	}
	regexMemoMu.Unlock()

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	// Insert under the lock. If this insert pushes us over the cap, evict
	// the oldest entry (by insertion order) rather than clearing the entire
	// map, avoiding cache thrashing when many distinct patterns are used in
	// quick succession.
	regexMemoMu.Lock()
	if _, ok := regexMemo[pattern]; !ok {
		if len(regexMemo) >= regexMemoMax && len(regexMemoOrder) > 0 {
			victim := regexMemoOrder[0]
			regexMemoOrder = regexMemoOrder[1:]
			delete(regexMemo, victim)
		}
		regexMemo[pattern] = re
		regexMemoOrder = append(regexMemoOrder, pattern)
	}
	regexMemoMu.Unlock()
	return re, nil
}
