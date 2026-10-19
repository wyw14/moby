package build

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/v2/daemon/builder"
	daemonevents "github.com/moby/moby/v2/daemon/events"
	"github.com/moby/moby/v2/daemon/server/buildbackend"
)

type fakeBuilder struct {
	result     *builder.Result
	buildErr   error
	phases     int
	blockOnCtx bool
}

func (f *fakeBuilder) Build(ctx context.Context, config buildbackend.BuildConfig) (*builder.Result, error) {
	for i := 1; i <= f.phases; i++ {
		cached := i%2 == 1
		digest := "sha256:layer"
		if cached {
			digest = "sha256:cached"
		}
		config.EventSink.Send(build.SummaryEvent{
			Type:      build.SummaryEventPhaseDone,
			Timestamp: time.Now().UTC(),
			Phase: &build.PhaseSummary{
				Phase:               build.PhaseStep,
				Number:              i,
				Command:             "RUN echo",
				Cached:              cached,
				DurationNanoseconds: int64(i),
				LayerDigest:         digest,
			},
		})
	}
	if f.blockOnCtx {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
	return f.result, f.buildErr
}

func drainEvents(t *testing.T, ch <-chan build.SummaryEvent, want int, timeout time.Duration) []build.SummaryEvent {
	t.Helper()
	var got []build.SummaryEvent
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
	return got
}

func newTestBackend(t *testing.T, fb *fakeBuilder) *Backend {
	t.Helper()
	b, err := NewBackend(nil, fb, nil, daemonevents.New())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func buildConfig(buildID string, imageID string) buildbackend.BuildConfig {
	return buildbackend.BuildConfig{
		Options: &buildbackend.BuildOptions{
			BuildID: buildID,
			Version: build.BuilderV1,
		},
		ProgressWriter: buildbackend.ProgressWriter{
			Output:          io.Discard,
			StdoutFormatter: io.Discard,
			StderrFormatter: io.Discard,
		},
	}
}

func TestBuildSummaryEventsSuccess(t *testing.T) {
	fb := &fakeBuilder{result: &builder.Result{ImageID: "sha256:image"}, phases: 2}
	b := newTestBackend(t, fb)

	ch, cancel := b.SubscribeBuildEvents("id-1")
	defer cancel()

	imageID, err := b.Build(t.Context(), buildConfig("id-1", "sha256:image"))
	if err != nil {
		t.Fatal(err)
	}
	if imageID != "sha256:image" {
		t.Fatalf("legacy return value changed: %q", imageID)
	}

	evs := drainEvents(t, ch, 4, time.Second)
	if len(evs) != 4 {
		t.Fatalf("expected started + 2 phases + done (4 events), got %d", len(evs))
	}
	if evs[0].Type != build.SummaryEventBuildStarted || evs[0].BuildID != "id-1" {
		t.Fatalf("unexpected first event: %+v", evs[0])
	}
	for i, ev := range evs[1:3] {
		if ev.Type != build.SummaryEventPhaseDone || ev.Phase == nil || ev.Phase.Number != i+1 {
			t.Fatalf("unexpected phase event %d: %+v", i, ev)
		}
	}
	done := evs[3]
	if done.Type != build.SummaryEventBuildDone || done.Summary == nil {
		t.Fatalf("unexpected last event: %+v", done)
	}
	s := done.Summary
	if s.ImageID != "sha256:image" || s.TotalSteps != 2 || s.CachedSteps != 1 {
		t.Fatalf("unexpected summary: %+v", s)
	}
	if len(s.Layers) != 2 {
		t.Fatalf("unexpected layers: %+v", s.Layers)
	}
}

func TestBuildSummaryEventsFailureReason(t *testing.T) {
	fb := &fakeBuilder{buildErr: errors.New("boom"), phases: 1}
	b := newTestBackend(t, fb)

	ch, cancel := b.SubscribeBuildEvents("id-fail")
	defer cancel()

	imageID, err := b.Build(t.Context(), buildConfig("id-fail", ""))
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected boom error, got %v", err)
	}
	if imageID != "" {
		t.Fatalf("failed build must not return an image ID, got %q", imageID)
	}

	evs := drainEvents(t, ch, 3, time.Second)
	if len(evs) != 3 {
		t.Fatalf("expected 3 events, got %d", len(evs))
	}
	done := evs[len(evs)-1]
	if done.Type != build.SummaryEventBuildDone || done.Summary.Error != "boom" || done.Summary.ImageID != "" {
		t.Fatalf("unexpected done event: %+v", done)
	}
}

func TestBuildCanceledReportsContextCanceled(t *testing.T) {
	fb := &fakeBuilder{blockOnCtx: true}
	b := newTestBackend(t, fb)

	ch, cancelSub := b.SubscribeBuildEvents("id-cancel")
	defer cancelSub()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := b.Build(ctx, buildConfig("id-cancel", ""))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	evs := drainEvents(t, ch, 2, time.Second)
	if len(evs) != 2 {
		t.Fatalf("expected started + done, got %d", len(evs))
	}
	done := evs[len(evs)-1]
	if done.Type != build.SummaryEventBuildDone || done.Summary.Error != "context canceled" {
		t.Fatalf("unexpected done event: %+v", done)
	}
}

// A subscriber that disconnects before (or while) a build runs must not
// affect the build at all.
func TestBuildWithDisconnectedSubscriber(t *testing.T) {
	fb := &fakeBuilder{result: &builder.Result{ImageID: "sha256:img"}, phases: 1000}
	b := newTestBackend(t, fb)

	_, cancelSub := b.SubscribeBuildEvents("id-disc")
	cancelSub() // disconnect before the build starts

	imageID, err := b.Build(t.Context(), buildConfig("id-disc", "sha256:img"))
	if err != nil {
		t.Fatalf("build failed after subscriber disconnected: %v", err)
	}
	if imageID != "sha256:img" {
		t.Fatalf("unexpected image ID: %q", imageID)
	}
}

// Many builds with their own subscribers run concurrently; each subscriber
// only sees its own build's events, the legacy return values are preserved
// and builds with no subscriber work as well.
func TestConcurrentBuilds(t *testing.T) {
	const n = 25
	fb := &fakeBuilder{result: &builder.Result{ImageID: "sha256:img"}, phases: 3}
	b := newTestBackend(t, fb)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		id := "concurrent-" + itoa(i)
		ch, cancelSub := b.SubscribeBuildEvents(id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancelSub()

			cfg := buildConfig(id, "sha256:img")
			imageID, err := b.Build(t.Context(), cfg)
			if err != nil {
				t.Errorf("build %s failed: %v", id, err)
				return
			}
			if imageID != "sha256:img" {
				t.Errorf("build %s legacy return changed: %q", id, imageID)
			}

			// started + 3 phases + done
			evs := drainEvents(t, ch, 5, 5*time.Second)
			if len(evs) != 5 {
				t.Errorf("build %s expected 5 events, got %d", id, len(evs))
				return
			}
			for _, ev := range evs {
				if ev.BuildID != id {
					t.Errorf("build %s received foreign event for %q", id, ev.BuildID)
				}
			}
			if evs[len(evs)-1].Summary.ImageID != "sha256:img" {
				t.Errorf("build %s unexpected final summary: %+v", id, evs[len(evs)-1].Summary)
			}
		}()
	}

	// An unrelated build with no subscriber must still run unchanged.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := b.Build(t.Context(), buildConfig("no-subscriber", "sha256:img")); err != nil {
			t.Errorf("build without subscriber failed: %v", err)
		}
	}()
	wg.Wait()
}

// The legacy progress output is byte-for-byte still produced when no
// summary subscriber is attached.
func TestLegacyOutputUnchanged(t *testing.T) {
	out := &bytes.Buffer{}
	fb := &fakeBuilder{result: &builder.Result{ImageID: "sha256:abcdef0123456789"}}
	b := newTestBackend(t, fb)

	cfg := buildConfig("legacy", "sha256:abcdef0123456789")
	cfg.ProgressWriter.StdoutFormatter = out
	if _, err := b.Build(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "Successfully built abcdef012345\n" {
		t.Fatalf("legacy output changed: %q", got)
	}
}

// Events of a build with an auto-generated ID are still observable via the
// wildcard ("") subscription.
func TestWildcardSubscriptionReceivesGeneratedID(t *testing.T) {
	fb := &fakeBuilder{result: &builder.Result{ImageID: "sha256:img"}}
	b := newTestBackend(t, fb)

	ch, cancelSub := b.SubscribeBuildEvents("")
	defer cancelSub()

	cfg := buildConfig("", "sha256:img") // empty BuildID -> generated
	if _, err := b.Build(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	evs := drainEvents(t, ch, 2, time.Second)
	if len(evs) != 2 || evs[0].BuildID == "" {
		t.Fatalf("expected started/done events with a generated ID, got %+v", evs)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
