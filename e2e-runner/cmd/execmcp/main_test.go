package main

import (
	"io"
	"strings"
	"testing"
)

// The cap is only safe because the dropped bytes survive on disk, so the
// two things worth pinning are: the head is kept exactly, and the
// MultiWriter fan-out to the log file is not aborted by a short write.
func TestHeadBuffer(t *testing.T) {
	t.Run("keeps head and counts the rest", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			max         int
			writes      []string
			wantKept    string
			wantDropped int
		}{
			{"under cap", 10, []string{"abc"}, "abc", 0},
			{"exactly at cap", 3, []string{"abc"}, "abc", 0},
			{"single oversized write splits", 3, []string{"abcde"}, "abc", 2},
			{"cap straddled across writes", 4, []string{"ab", "cdef"}, "abcd", 2},
			{"writes after cap are all dropped", 2, []string{"ab", "cd", "ef"}, "ab", 4},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := &headBuffer{max: tc.max}
				for _, w := range tc.writes {
					n, err := h.Write([]byte(w))
					if err != nil {
						t.Fatalf("Write(%q) returned error: %v", w, err)
					}
					// A short count would make io.MultiWriter give up.
					if n != len(w) {
						t.Fatalf("Write(%q) = %d, want %d (short write aborts MultiWriter)", w, n, len(w))
					}
				}
				if got := h.buf.String(); got != tc.wantKept {
					t.Errorf("kept %q, want %q", got, tc.wantKept)
				}
				if h.dropped != tc.wantDropped {
					t.Errorf("dropped %d, want %d", h.dropped, tc.wantDropped)
				}
			})
		}
	})

	// The whole recovery path depends on the log file still receiving
	// everything after the in-context buffer has filled up.
	t.Run("MultiWriter keeps feeding the log after the cap", func(t *testing.T) {
		h := &headBuffer{max: 4}
		var full strings.Builder
		w := io.MultiWriter(h, &full)

		for _, chunk := range []string{"aaaa", "bbbb", "cccc"} {
			if _, err := w.Write([]byte(chunk)); err != nil {
				t.Fatalf("MultiWriter.Write(%q): %v", chunk, err)
			}
		}
		if got := h.buf.String(); got != "aaaa" {
			t.Errorf("in-context buffer = %q, want %q", got, "aaaa")
		}
		if got := full.String(); got != "aaaabbbbcccc" {
			t.Errorf("full log = %q, want the complete stream", got)
		}
		if h.dropped != 8 {
			t.Errorf("dropped = %d, want 8", h.dropped)
		}
	})
}
