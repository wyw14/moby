package builder

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/build"
)

func phaseEvent(buildID string, n int, cached bool, digest string) build.SummaryEvent {
	return build.SummaryEvent{
		Type:      build.SummaryEventPhaseDone,
		Timestamp: time.Now().UTC(),
		BuildID:   buildID,
		Phase: &build.PhaseSummary{
			Phase:               build.PhaseStep,
			Number:              n,
			Command:             "RUN echo",
			Cached:              cached,
			DurationNanoseconds: int64(n),
			LayerDigest:         digest,
		},
	}
}

func drain(t *testing.T, ch <-chan build.SummaryEvent, want int, timeout time.Duration) []build.SummaryEvent {
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

func TestEventHubFiltersByBuildID(t *testing.T) {
	hub := NewEventHub()

	a, cancelA := hub.Subscribe("a")
	defer cancelA()
	b, cancelB := hub.Subscribe("b")
	defer cancelB()
	all, cancelAll := hub.Subscribe("")
	defer cancelAll()

	hub.Publish("a", phaseEvent("a", 1, true, "sha256:a1"))
	hub.Publish("b", phaseEvent("b", 1, false, "sha256:b1"))

	evsA := drain(t, a, 1, time.Second)
	if len(evsA) != 1 || evsA[0].BuildID != "a" {
		t.Fatalf("subscriber a got %+v", evsA)
	}
	evsB := drain(t, b, 1, time.Second)
	if len(evsB) != 1 || evsB[0].BuildID != "b" {
		t.Fatalf("subscriber b got %+v", evsB)
	}
	if evs := drain(t, all, 2, time.Second); len(evs) != 2 {
		t.Fatalf("wildcard subscriber expected 2 events, got %d", len(evs))
	}
}

// A subscriber that disconnects must not block publishes and the channel
// must not receive (or panic on) later events.
func TestEventHubDisconnectDoesNotBlock(t *testing.T) {
	hub := NewEventHub()
	ch, cancel := hub.Subscribe("x")
	cancel()

	if _, ok := <-ch; ok {
		t.Fatal("expected channel to be closed after cancel")
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			hub.Publish("x", phaseEvent("x", i, false, "sha256:x"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing to a disconnected subscriber blocked the build")
	}

	// cancel is idempotent.
	cancel()
}

// A slow subscriber (never reads) must not block publishes once its buffer
// fills; events are dropped.
func TestEventHubSlowSubscriberDropsEvents(t *testing.T) {
	hub := NewEventHub()
	ch, cancel := hub.Subscribe("x")
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < EventBufferSize*10; i++ {
			hub.Publish("x", phaseEvent("x", i, false, "sha256:x"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing to a slow subscriber blocked the build")
	}
	if len(ch) > EventBufferSize {
		t.Fatalf("subscriber channel exceeded buffer: %d", len(ch))
	}
}

func TestEventHubConcurrentBuildsAndSubscriptions(t *testing.T) {
	hub := NewEventHub()
	const builds = 32
	const eventsPerBuild = 50

	var wg sync.WaitGroup
	// Churn subscribers concurrently with publishers.
	for i := 0; i < builds; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "build-" + itoa(i)
			ch, cancel := hub.Subscribe(id)
			for j := 0; j < eventsPerBuild; j++ {
				hub.Publish(id, phaseEvent(id, j, j%2 == 0, "sha256:layer"))
				if j%10 == 0 {
					time.Sleep(time.Microsecond)
				}
			}
			got := drain(t, ch, eventsPerBuild, 5*time.Second)
			if len(got) == 0 {
				t.Errorf("build %s subscriber got no events", id)
			}
			for _, ev := range got {
				if ev.BuildID != id {
					t.Errorf("build %s received event for %s", id, ev.BuildID)
				}
			}
			cancel()
		}()
	}
	// Unrelated subscribe/unsubscribe churn.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, cancel := hub.Subscribe("nonexistent")
				cancel()
			}
		}
	}()
	wg.Wait()
	close(stop)
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

func TestRecorderAggregatesSummary(t *testing.T) {
	hub := NewEventHub()
	ch, cancel := hub.Subscribe("build-1")
	defer cancel()

	r := NewRecorder("build-1", hub.Sink("build-1"))
	r.Start()
	r.Send(phaseEvent("build-1", 1, true, "sha256:l1"))
	r.Send(phaseEvent("build-1", 2, false, "sha256:l2"))
	summary := r.Finish("sha256:image", nil)

	if summary.TotalSteps != 2 || summary.CachedSteps != 1 {
		t.Fatalf("unexpected step counts: %+v", summary)
	}
	if got := summary.Layers; len(got) != 2 || got[0] != "sha256:l1" || got[1] != "sha256:l2" {
		t.Fatalf("unexpected layers: %+v", got)
	}
	if summary.ImageID != "sha256:image" || summary.Error != "" {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.DurationNanoseconds < 0 {
		t.Fatalf("negative duration: %d", summary.DurationNanoseconds)
	}

	evs := drain(t, ch, 4, time.Second)
	if len(evs) != 4 {
		t.Fatalf("expected 4 events (started, 2 phases, done), got %d", len(evs))
	}
	if evs[0].Type != build.SummaryEventBuildStarted {
		t.Fatalf("first event = %s", evs[0].Type)
	}
	if evs[3].Type != build.SummaryEventBuildDone || evs[3].Summary == nil {
		t.Fatalf("last event = %+v", evs[3])
	}

	// Finish is idempotent: no second build-done event, same summary returned.
	again := r.Finish("other", errors.New("other"))
	if again.ImageID != "sha256:image" {
		t.Fatalf("idempotent Finish changed summary: %+v", again)
	}
	if extra := drain(t, ch, 1, 50*time.Millisecond); len(extra) != 0 {
		t.Fatalf("expected no more events, got %d", len(extra))
	}
}

func TestRecorderCanceledBuildReportsReason(t *testing.T) {
	hub := NewEventHub()
	ch, cancel := hub.Subscribe("c")
	defer cancel()

	r := NewRecorder("c", hub.Sink("c"))
	r.Start()
	ctx, cancelCtx := context.WithCancel(context.Background())
	cancelCtx()
	r.Send(phaseEvent("c", 1, false, "sha256:l1"))
	r.Send(build.SummaryEvent{
		Type:      build.SummaryEventPhaseDone,
		BuildID:   "c",
		Timestamp: time.Now().UTC(),
		Phase: &build.PhaseSummary{
			Phase:   build.PhaseStep,
			Number:  2,
			Command: "RUN sleep",
			Error:   "Build cancelled",
		},
	})
	summary := r.Finish("", ctx.Err())

	if summary.ImageID != "" {
		t.Fatalf("canceled build must not report an image: %+v", summary)
	}
	if summary.Error != "context canceled" {
		t.Fatalf("expected cancellation reason, got %q", summary.Error)
	}

	evs := drain(t, ch, 4, time.Second)
	last := evs[len(evs)-1]
	if last.Type != build.SummaryEventBuildDone || last.Summary.Error != "context canceled" {
		t.Fatalf("unexpected last event: %+v", last)
	}
}

func TestRecorderTruncatesFailureReason(t *testing.T) {
	longErr := errors.New(strings.Repeat("E", build.MaxSummaryTextLen*2))
	r := NewRecorder("big", nil)
	r.Start()
	summary := r.Finish("", longErr)
	if len(summary.Error) >= len(longErr.Error()) {
		t.Fatalf("failure reason not truncated: %d bytes", len(summary.Error))
	}
	if !strings.HasSuffix(summary.Error, "...(truncated)") {
		t.Fatalf("expected truncation marker, got %q", summary.Error[len(summary.Error)-30:])
	}
}

func TestRecorderNilSafe(t *testing.T) {
	var r *Recorder
	r.Start()
	r.Send(phaseEvent("x", 1, false, ""))
	if got := r.Finish("", errors.New("boom")); got.Error != "" {
		t.Fatalf("nil recorder should return zero summary, got %+v", got)
	}

	// Nil sink must not panic.
	r2 := NewRecorder("x", nil)
	r2.Start()
	r2.Send(phaseEvent("x", 1, false, ""))
	r2.Finish("img", nil)
}
