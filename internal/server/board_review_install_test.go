package server

import (
	"sync"
	"testing"
)

// TestInstallReviewAgentConcurrent pins the lazy install as idempotent and
// race-free. installReviewAgent runs OFF the WS read loop (a settings save
// persists + broadcasts in a goroutine, and applyBoardManagerToAll installs the
// trigger there), so two tabs saving settings at once did an unsynchronized
// read-modify-write of s.reviewTrigger: a data race under -race and, worse, two
// trigger objects where only the one installed last was hooked.
func TestInstallReviewAgentConcurrent(t *testing.T) {
	dir := t.TempDir()
	s, _, _ := newContinuationServer(t, newBlockingStub(), dir)
	s.ws.ensureBoardManager() // nil manager is a no-op, so the install must have one

	const (
		goroutines = 8
		rounds     = 50
	)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				s.installReviewAgent()
			}
		}()
	}
	wg.Wait()

	// Exactly one trigger exists, and every later call reuses it (read under
	// the same lock the install takes).
	s.reviewTriggerMu.Lock()
	first := s.reviewTrigger
	s.reviewTriggerMu.Unlock()
	if first == nil {
		t.Fatal("no review trigger installed")
	}
	s.installReviewAgent()
	s.reviewTriggerMu.Lock()
	second := s.reviewTrigger
	s.reviewTriggerMu.Unlock()
	if second != first {
		t.Fatal("installReviewAgent replaced the trigger instead of reusing it")
	}
}
