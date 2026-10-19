package dockerfile

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/builder"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/daemon/server/buildbackend"
)

type captureSink struct {
	events []build.SummaryEvent
}

func (s *captureSink) Send(ev build.SummaryEvent) {
	s.events = append(s.events, ev)
}

func (s *captureSink) phases() []build.PhaseSummary {
	var out []build.PhaseSummary
	for _, ev := range s.events {
		if ev.Type == build.SummaryEventPhaseDone && ev.Phase != nil {
			out = append(out, *ev.Phase)
		}
	}
	return out
}

func newEventBuilder(t *testing.T, mb *MockBackend, sink build.EventSink) *Builder {
	t.Helper()
	b, err := newBuilder(context.Background(), builderOptions{
		Options: &buildbackend.BuildOptions{},
		Backend: mb,
		ProgressWriter: buildbackend.ProgressWriter{
			Output:          io.Discard,
			StdoutFormatter: io.Discard,
			StderrFormatter: io.Discard,
		},
		EventSink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseDockerfile(t *testing.T, contents string) *parser.Result {
	t.Helper()
	result, err := parser.Parse(strings.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A cached ENV step must be reported as a phase-done event with cached=true
// and the cached layer digest, without running any container.
func TestBuildRecordsCachedPhase(t *testing.T) {
	mb := &MockBackend{
		makeImageCacheFunc: func([]string) builder.ImageCache {
			return &mockImageCache{getCacheFunc: func(string, *container.Config) (string, error) {
				return "sha256:cached-layer", nil
			}}
		},
	}
	sink := &captureSink{}
	b := newEventBuilder(t, mb, sink)

	result, err := b.build(t.Context(), nil, parseDockerfile(t, "FROM theid\nENV a=b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageID != "sha256:cached-layer" {
		t.Fatalf("unexpected image id %q", result.ImageID)
	}

	phases := sink.phases()
	if len(phases) != 1 {
		t.Fatalf("expected 1 phase event, got %d: %+v", len(phases), phases)
	}
	p := phases[0]
	if p.Phase != build.PhaseStep || p.Number != 2 { // step 1 is the FROM line
		t.Fatalf("unexpected phase identity: %+v", p)
	}
	if !p.Cached {
		t.Fatalf("expected cached phase, got %+v", p)
	}
	if p.LayerDigest != "sha256:cached-layer" {
		t.Fatalf("unexpected layer digest: %+v", p)
	}
	if !strings.Contains(p.Command, "ENV a=b") {
		t.Fatalf("unexpected command: %q", p.Command)
	}
	if p.Error != "" || p.DurationNanoseconds < 0 {
		t.Fatalf("unexpected phase outcome: %+v", p)
	}
}

// A cache miss that commits a new layer reports cached=false and the newly
// committed layer digest.
func TestBuildRecordsExecutedPhase(t *testing.T) {
	mb := &MockBackend{
		makeImageCacheFunc: func([]string) builder.ImageCache {
			return &mockImageCache{getCacheFunc: func(string, *container.Config) (string, error) {
				return "", nil // cache miss
			}}
		},
		containerCreateFunc: func(backend.ContainerCreateConfig) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "intermediate-container"}, nil
		},
		commitFunc: func(backend.CommitConfig) (image.ID, error) {
			return image.ID("sha256:new-layer"), nil
		},
	}
	sink := &captureSink{}
	b := newEventBuilder(t, mb, sink)

	if _, err := b.build(t.Context(), nil, parseDockerfile(t, "FROM theid\nENV a=b\n")); err != nil {
		t.Fatal(err)
	}

	phases := sink.phases()
	if len(phases) != 1 {
		t.Fatalf("expected 1 phase event, got %d", len(phases))
	}
	p := phases[0]
	if p.Cached {
		t.Fatalf("expected executed (non-cached) phase, got %+v", p)
	}
	if p.LayerDigest != "sha256:new-layer" {
		t.Fatalf("unexpected layer digest: %+v", p)
	}
}

// Failure reasons and long instructions are truncated so the summary stays
// bounded regardless of build log size.
func TestRecordStepTruncatesCommandAndError(t *testing.T) {
	sink := &captureSink{}
	b := &Builder{eventSink: sink}

	longRUN := "RUN " + strings.Repeat("x", build.MaxSummaryTextLen*2)
	result := parseDockerfile(t, longRUN)
	cmd, err := instructions.ParseCommand(result.AST.Children[0])
	if err != nil {
		t.Fatal(err)
	}

	b.recordStep(1, "stage", cmd, time.Millisecond, errors.New(strings.Repeat("E", build.MaxSummaryTextLen*2)), "sha256:x")

	phases := sink.phases()
	if len(phases) != 1 {
		t.Fatalf("expected 1 phase event, got %d", len(phases))
	}
	p := phases[0]
	if len(p.Command) > build.MaxSummaryTextLen+len("...(truncated)") {
		t.Fatalf("command not truncated: %d bytes", len(p.Command))
	}
	if !strings.HasSuffix(p.Command, "...(truncated)") {
		t.Fatalf("command missing truncation marker")
	}
	if len(p.Error) > build.MaxSummaryTextLen+len("...(truncated)") ||
		!strings.HasSuffix(p.Error, "...(truncated)") {
		t.Fatalf("error not truncated: %d bytes", len(p.Error))
	}
}

// A build without an event sink behaves exactly as before; instrumentation
// must not panic.
func TestRecordStepNilSink(t *testing.T) {
	b := &Builder{} // eventSink is nil
	result := parseDockerfile(t, "RUN echo hello")
	cmd, err := instructions.ParseCommand(result.AST.Children[0])
	if err != nil {
		t.Fatal(err)
	}
	b.recordStep(1, "", cmd, time.Millisecond, nil, "sha256:x")
}

// A build whose context is already canceled stops at the next step and
// returns the cancellation error without emitting step events for work that
// never ran.
func TestBuildCanceledBeforeStep(t *testing.T) {
	mb := &MockBackend{
		makeImageCacheFunc: func([]string) builder.ImageCache {
			return &mockImageCache{getCacheFunc: func(string, *container.Config) (string, error) {
				return "sha256:cached-layer", nil
			}}
		},
	}
	sink := &captureSink{}
	b := newEventBuilder(t, mb, sink)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := b.build(ctx, nil, parseDockerfile(t, "FROM theid\nENV a=b\nENV c=d\n"))
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("expected cancellation error, got %v", err)
	}
	for _, p := range sink.phases() {
		if p.Cached {
			t.Fatalf("canceled build must not record executed/cached steps, got %+v", p)
		}
	}
}
