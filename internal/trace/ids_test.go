package trace

import "testing"

func TestNewTraceID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newTraceID()
		if err != nil {
			t.Fatalf("newTraceID: %v", err)
		}
		if len(id) != 32 {
			t.Fatalf("trace id %q: got length %d, want 32", id, len(id))
		}
		if seen[id] {
			t.Fatalf("trace id %q generated twice in %d iterations", id, i)
		}
		seen[id] = true
	}
}

func TestNewSpanID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newSpanID()
		if err != nil {
			t.Fatalf("newSpanID: %v", err)
		}
		if len(id) != 16 {
			t.Fatalf("span id %q: got length %d, want 16", id, len(id))
		}
		if seen[id] {
			t.Fatalf("span id %q generated twice in %d iterations", id, i)
		}
		seen[id] = true
	}
}
