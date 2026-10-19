package builder

import (
	"sync"
	"time"

	"github.com/moby/moby/api/types/build"
)

// EventBufferSize is the per-subscriber capacity of build summary event
// channels. Events published while the buffer is full (a slow or stalled
// consumer) are dropped, so builds never block on subscribers.
const EventBufferSize = 256

// EventHub is a non-blocking publish/subscribe hub for build summary events.
//
// Publishing never blocks: events are dropped for subscribers whose channel
// is full, and a subscriber that unsubscribes (disconnects) simply stops
// receiving events. The zero value is not usable; use NewEventHub.
type EventHub struct {
	mu     sync.RWMutex
	nextID uint64
	subs   map[uint64]*hubSubscription
}

type hubSubscription struct {
	buildID string // empty buildID subscribes to all builds
	ch      chan build.SummaryEvent
}

// NewEventHub creates an empty EventHub.
func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[uint64]*hubSubscription)}
}

// Subscribe registers a subscriber for the summary events of buildID. An
// empty buildID subscribes to events of all builds. It returns the event
// channel and a cancel function that unsubscribes and closes the channel.
//
// The cancel function is safe to call more than once and may be called by a
// disconnected subscriber at any time; doing so never blocks a build.
func (h *EventHub) Subscribe(buildID string) (<-chan build.SummaryEvent, func()) {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	ch := make(chan build.SummaryEvent, EventBufferSize)
	h.subs[id] = &hubSubscription{buildID: buildID, ch: ch}
	h.mu.Unlock()

	cancel := sync.OnceFunc(func() {
		h.mu.Lock()
		if sub, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(sub.ch)
		}
		h.mu.Unlock()
	})
	return ch, cancel
}

// Publish broadcasts an event to the matching subscribers. It never blocks
// on a slow subscriber (events for full channels are dropped) and never sends
// on an unsubscribed (closed) channel.
func (h *EventHub) Publish(buildID string, ev build.SummaryEvent) {
	h.mu.RLock()
	for _, sub := range h.subs {
		if sub.buildID != "" && sub.buildID != buildID {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// Subscriber is too slow or has stopped reading; drop the event
			// rather than stalling the build.
		}
	}
	h.mu.RUnlock()
}

// Sink returns an EventSink that tags events with buildID and publishes them
// to the hub.
func (h *EventHub) Sink(buildID string) build.EventSink {
	return hubSink{hub: h, buildID: buildID}
}

type hubSink struct {
	hub     *EventHub
	buildID string
}

func (s hubSink) Send(ev build.SummaryEvent) {
	if ev.BuildID == "" {
		ev.BuildID = s.buildID
	}
	s.hub.Publish(s.buildID, ev)
}

// discardSink is an EventSink that drops every event.
var discardSink build.EventSink = nopSink{}

type nopSink struct{}

func (nopSink) Send(build.SummaryEvent) {}

// Recorder aggregates the summary of a single build and forwards every event
// to an EventSink. It implements build.EventSink, so builders can report
// phase events through it directly. All methods are safe for concurrent use.
//
// Forwarding is non-blocking when the sink is one returned by EventHub; a nil
// sink discards every event.
type Recorder struct {
	buildID string
	sink    build.EventSink

	mu      sync.Mutex
	started bool
	done    bool
	summary build.BuildSummary
}

// NewRecorder creates a Recorder for buildID. A nil sink discards events.
func NewRecorder(buildID string, sink build.EventSink) *Recorder {
	if sink == nil {
		sink = discardSink
	}
	return &Recorder{
		buildID: buildID,
		sink:    sink,
		summary: build.BuildSummary{BuildID: buildID},
	}
}

// Start publishes a SummaryEventBuildStarted event. It is a no-op if the
// recorder was already started.
func (r *Recorder) Start() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	now := time.Now().UTC()
	r.summary.StartedAt = now
	r.mu.Unlock()

	r.sink.Send(build.SummaryEvent{
		Type:      build.SummaryEventBuildStarted,
		Timestamp: now,
		BuildID:   r.buildID,
	})
}

// Send records a phase event into the aggregate summary and forwards it to
// the sink. Other event types are forwarded unchanged.
func (r *Recorder) Send(ev build.SummaryEvent) {
	if r == nil {
		return
	}
	if ev.BuildID == "" {
		ev.BuildID = r.buildID
	}
	if ev.Type == build.SummaryEventPhaseDone && ev.Phase != nil {
		r.mu.Lock()
		r.summary.TotalSteps++
		if ev.Phase.Cached {
			r.summary.CachedSteps++
		}
		if ev.Phase.LayerDigest != "" {
			r.summary.Layers = append(r.summary.Layers, ev.Phase.LayerDigest)
		}
		r.mu.Unlock()
	}
	r.sink.Send(ev)
}

// Finish finalizes the aggregate summary with the resulting image ID and
// build error (nil on success), publishes a SummaryEventBuildDone event, and
// returns the summary. It is safe to call more than once: subsequent calls
// return the finalized summary without publishing another event.
func (r *Recorder) Finish(imageID string, buildErr error) build.BuildSummary {
	if r == nil {
		return build.BuildSummary{}
	}
	r.mu.Lock()
	if r.done {
		s := r.summary
		r.mu.Unlock()
		return s
	}
	r.done = true

	if r.started {
		r.summary.DurationNanoseconds = time.Since(r.summary.StartedAt).Nanoseconds()
	}
	r.summary.ImageID = imageID
	if buildErr != nil {
		r.summary.Error = build.TruncateText(buildErr.Error())
	}
	summary := r.summary
	// Copy the layer slice so the summary handed to subscribers does not
	// share its backing array with late phase events.
	if r.summary.Layers != nil {
		summary.Layers = append([]string(nil), r.summary.Layers...)
	}
	r.mu.Unlock()

	r.sink.Send(build.SummaryEvent{
		Type:      build.SummaryEventBuildDone,
		Timestamp: time.Now().UTC(),
		BuildID:   r.buildID,
		Summary:   &summary,
	})
	return summary
}
