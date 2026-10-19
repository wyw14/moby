package build

import (
	"time"
	"unicode/utf8"
)

// SummaryEventType is the type of a build summary event.
type SummaryEventType string

const (
	// SummaryEventBuildStarted is emitted once, before the build starts.
	SummaryEventBuildStarted SummaryEventType = "build-started"
	// SummaryEventPhaseDone is emitted when a single build phase (for example
	// a Dockerfile instruction) finishes, whether it was executed, served
	// from cache, or failed.
	SummaryEventPhaseDone SummaryEventType = "phase-done"
	// SummaryEventBuildDone is emitted once, when the build finishes, and
	// carries the aggregated build summary.
	SummaryEventBuildDone SummaryEventType = "build-done"
)

// SummaryPhase identifies the build phase a PhaseSummary describes.
type SummaryPhase string

const (
	// PhaseStep is a single Dockerfile instruction.
	PhaseStep SummaryPhase = "step"
)

// MaxSummaryTextLen is the maximum length, in bytes, of free-form text fields
// in a summary (instruction source code, failure reason). Longer text is
// truncated so the summary JSON stays bounded regardless of build log size.
const MaxSummaryTextLen = 4096

const truncationSuffix = "...(truncated)"

// TruncateText truncates s to MaxSummaryTextLen bytes, appending a marker
// when truncation happened. The cut point is adjusted to stay on a valid
// UTF-8 boundary.
func TruncateText(s string) string {
	if len(s) <= MaxSummaryTextLen {
		return s
	}
	head := s[:MaxSummaryTextLen]
	if r, size := utf8.DecodeLastRuneInString(head); r == utf8.RuneError && size > 0 {
		head = head[:len(head)-size]
	}
	return head + truncationSuffix
}

// SummaryEvent is a single event in a build summary event stream.
//
// The JSON representation is stable: field names and types do not change
// between releases; new fields may only be added.
type SummaryEvent struct {
	// Type is the kind of the event.
	Type SummaryEventType `json:"type"`
	// Timestamp is the time at which the event was produced (UTC).
	Timestamp time.Time `json:"timestamp"`
	// BuildID identifies the build the event belongs to.
	BuildID string `json:"build_id,omitempty"`
	// Phase is set for SummaryEventPhaseDone events.
	Phase *PhaseSummary `json:"phase,omitempty"`
	// Summary is set for SummaryEventBuildDone events.
	Summary *BuildSummary `json:"summary,omitempty"`
}

// PhaseSummary describes the outcome of a single build phase.
type PhaseSummary struct {
	// Phase is the phase kind.
	Phase SummaryPhase `json:"phase"`
	// Number is the 1-based, monotonically increasing position of the phase
	// within the build.
	Number int `json:"number"`
	// Stage is the name of the build stage the phase belongs to, if named.
	Stage string `json:"stage,omitempty"`
	// Command is the source code of the instruction, truncated to
	// MaxSummaryTextLen bytes.
	Command string `json:"command,omitempty"`
	// Cached reports whether the phase was served from the build cache.
	Cached bool `json:"cached"`
	// DurationNanoseconds is the wall-clock time spent in the phase.
	DurationNanoseconds int64 `json:"duration_nanoseconds"`
	// LayerDigest is the digest of the image layer produced or reused by the
	// phase.
	LayerDigest string `json:"layer_digest,omitempty"`
	// Error is the truncated failure reason, empty on success.
	Error string `json:"error,omitempty"`
}

// BuildSummary is the aggregated summary of a whole build, carried by
// SummaryEventBuildDone events.
type BuildSummary struct {
	// BuildID identifies the build.
	BuildID string `json:"build_id,omitempty"`
	// StartedAt is the time the build started (UTC).
	StartedAt time.Time `json:"started_at"`
	// DurationNanoseconds is the wall-clock time spent building.
	DurationNanoseconds int64 `json:"duration_nanoseconds"`
	// TotalSteps is the number of recorded step phases.
	TotalSteps int `json:"total_steps"`
	// CachedSteps is the number of step phases served from cache.
	CachedSteps int `json:"cached_steps"`
	// Layers contains the digests of image layers produced or reused by the
	// steps, in execution order.
	Layers []string `json:"layers,omitempty"`
	// ImageID is the ID of the image produced by a successful build.
	ImageID string `json:"image_id,omitempty"`
	// Error is the truncated failure reason of a failed or canceled build.
	Error string `json:"error,omitempty"`
}

// EventSink receives build summary events. Implementations MUST NOT block:
// a slow or disconnected consumer must never stall the build that emits the
// event.
type EventSink interface {
	Send(SummaryEvent)
}
