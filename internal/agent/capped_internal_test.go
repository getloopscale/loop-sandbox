package agent

import "testing"

// White-box test of the output cap that stops untrusted code from OOMing the agent.
// It is pure Go (no VM, no subprocess) — the highest-value isolated-logic guard.
func TestCappedBuffer_truncates_at_limit_and_reports_full_write(t *testing.T) {
	c := &cappedBuffer{limit: 10}

	// Under the limit: stored fully, no truncation, full count returned.
	if n, _ := c.Write([]byte("abcd")); n != 4 {
		t.Fatalf("write 4 → n=%d, want 4", n)
	}
	if c.truncated {
		t.Errorf("truncated after under-limit write")
	}

	// Crossing the limit (room=6, write 8): the first 6 are kept, 2 dropped, but the
	// caller sees a FULL write (so the child is not disrupted with a short-write/
	// EPIPE), and truncated is set.
	if n, _ := c.Write([]byte("efghijKL")); n != 8 {
		t.Errorf("write 8 across cap → n=%d, want 8 (full report)", n)
	}
	if c.Len() != 10 {
		t.Errorf("buffered = %d, want 10 (capped)", c.Len())
	}
	if !c.truncated {
		t.Errorf("truncated not set after crossing the cap")
	}

	// Past the cap: dropped, still a full-count no-op, truncated stays set.
	if n, _ := c.Write([]byte("xyz")); n != 3 {
		t.Errorf("write past cap → n=%d, want 3", n)
	}
	if c.Len() != 10 {
		t.Errorf("buffered = %d, want 10 (unchanged)", c.Len())
	}
	if string(c.Bytes()) != "abcdefghij" {
		t.Errorf("content = %q, want 'abcdefghij'", c.Bytes())
	}
}
