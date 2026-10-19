package build

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTruncateText(t *testing.T) {
	t.Run("short unchanged", func(t *testing.T) {
		s := "short instruction"
		if got := TruncateText(s); got != s {
			t.Fatalf("expected %q unchanged, got %q", s, got)
		}
	})

	t.Run("exact limit unchanged", func(t *testing.T) {
		s := strings.Repeat("a", MaxSummaryTextLen)
		if got := TruncateText(s); got != s {
			t.Fatalf("expected string of %d bytes unchanged, got %d", MaxSummaryTextLen, len(got))
		}
	})

	t.Run("long truncated", func(t *testing.T) {
		s := strings.Repeat("x", MaxSummaryTextLen*2)
		got := TruncateText(s)
		if len(got) >= len(s) {
			t.Fatalf("expected truncation, got same length %d", len(got))
		}
		if !strings.HasSuffix(got, truncationSuffix) {
			t.Fatalf("expected truncation marker, got %q", got[len(got)-40:])
		}
		if !strings.HasPrefix(got, strings.Repeat("x", MaxSummaryTextLen)) {
			t.Fatalf("expected the first %d bytes to be preserved", MaxSummaryTextLen)
		}
	})

	t.Run("valid utf8 boundary", func(t *testing.T) {
		s := strings.Repeat("界", MaxSummaryTextLen) // each rune is 3 bytes
		got := TruncateText(s)
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("truncated text is not JSON-safe: %v", err)
		}
		if i := strings.Index(got, truncationSuffix); i >= 0 {
			if !strings.HasPrefix(got[:i], strings.Repeat("界", i/len("界"))) {
				t.Fatalf("truncation split a multi-byte rune")
			}
		}
	})
}

func TestSummaryEventStableJSON(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	ev := SummaryEvent{
		Type:      SummaryEventPhaseDone,
		Timestamp: ts,
		BuildID:   "build-1",
		Phase: &PhaseSummary{
			Phase:               PhaseStep,
			Number:              2,
			Stage:               "builder",
			Command:             "RUN echo hello",
			Cached:              true,
			DurationNanoseconds: 12345,
			LayerDigest:         "sha256:0123",
		},
	}

	first, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("marshaling is not deterministic:\n%s\n%s", first, second)
	}

	want := `{"type":"phase-done","timestamp":"2026-01-02T03:04:05.000000006Z","build_id":"build-1","phase":{"phase":"step","number":2,"stage":"builder","command":"RUN echo hello","cached":true,"duration_nanoseconds":12345,"layer_digest":"sha256:0123"}}`
	if string(first) != want {
		t.Fatalf("unexpected JSON:\n got: %s\nwant: %s", first, want)
	}
}

func TestBuildSummaryEmptyOmitsOptionals(t *testing.T) {
	b, err := json.Marshal(BuildSummary{StartedAt: time.Time{}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"started_at":"0001-01-01T00:00:00Z","duration_nanoseconds":0,"total_steps":0,"cached_steps":0}`
	if string(b) != want {
		t.Fatalf("unexpected JSON: %s, want %s", b, want)
	}
}
