package session

import "testing"

// TestStoreSetRetention verifies the runtime retention swap and its
// normalization (0 = default, negative maxAgeDays = keep forever).
func TestStoreSetRetention(t *testing.T) {
	s := NewStore(true)
	s.SetRetention(3, 7)
	if s.MaxCount() != 3 || s.MaxAgeDays() != 7 {
		t.Fatalf("retention after set = %d/%d, want 3/7", s.MaxCount(), s.MaxAgeDays())
	}
	s.SetRetention(0, -1) // defaults + keep forever
	if s.MaxCount() == 0 {
		t.Fatal("maxCount 0 should normalize to the default")
	}
	if s.MaxAgeDays() != -1 {
		t.Fatalf("maxAgeDays = %d, want -1 (keep forever)", s.MaxAgeDays())
	}
	s.SetRetention(5, 0) // explicit 0 age → default
	if s.MaxCount() != 5 {
		t.Fatalf("maxCount = %d, want 5", s.MaxCount())
	}
	if s.MaxAgeDays() <= 0 {
		t.Fatalf("maxAgeDays 0 should normalize to the default, got %d", s.MaxAgeDays())
	}
}
