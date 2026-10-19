package logstream_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/daemon/server/httputils/logstream"
	"gotest.tools/v3/assert"
)

// responseRecorder is an http.ResponseWriter + http.Flusher that captures
// everything written to the response.
type responseRecorder struct {
	header http.Header
	buf    bytes.Buffer
}

func (r *responseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {}

func (r *responseRecorder) Flush() {}

func (r *responseRecorder) Write(p []byte) (int, error) {
	return r.buf.Write(p)
}

func msg(source, line string) *backend.LogMessage {
	return &backend.LogMessage{
		Source:    source,
		Line:      []byte(line),
		Timestamp: time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC),
	}
}

// feed sends all messages into a buffered channel and closes it, so the
// producer never blocks even when the consumer stops reading early.
func feed(messages ...*backend.LogMessage) <-chan *backend.LogMessage {
	ch := make(chan *backend.LogMessage, len(messages))
	for _, m := range messages {
		ch <- m
	}
	close(ch)
	return ch
}

func TestWriteMaxBytesRaw(t *testing.T) {
	// TTY / non-multiplexed stream: bodies are written as-is, in order.
	for _, tc := range []struct {
		name     string
		config   backend.ContainerLogsOptions
		messages []*backend.LogMessage
		expected string
	}{
		{
			name:     "zero means unlimited",
			config:   backend.ContainerLogsOptions{ShowStdout: true},
			messages: []*backend.LogMessage{msg("stdout", "aaaaa"), msg("stdout", "bbbbb")},
			expected: "aaaaabbbbb",
		},
		{
			name:     "stops before the record that would exceed",
			config:   backend.ContainerLogsOptions{ShowStdout: true, MaxBytes: 10},
			messages: []*backend.LogMessage{msg("stdout", "aaaaa"), msg("stdout", "bbbbb"), msg("stdout", "ccccc")},
			expected: "aaaaabbbbb",
		},
		{
			name: "does not skip a non-fitting record to pick later smaller ones",
			// "aaaaa"+"bbbbb" use 10 of 11 bytes; "ccccc" would exceed, so the
			// response must end even though "dd" would fit.
			config:   backend.ContainerLogsOptions{ShowStdout: true, MaxBytes: 11},
			messages: []*backend.LogMessage{msg("stdout", "aaaaa"), msg("stdout", "bbbbb"), msg("stdout", "ccccc"), msg("stdout", "dd")},
			expected: "aaaaabbbbb",
		},
		{
			name:     "record equal to the remaining budget fits",
			config:   backend.ContainerLogsOptions{ShowStdout: true, MaxBytes: 5},
			messages: []*backend.LogMessage{msg("stdout", "aa"), msg("stdout", "bbb"), msg("stdout", "x")},
			expected: "aabbb",
		},
		{
			name:     "first record larger than budget writes nothing",
			config:   backend.ContainerLogsOptions{ShowStdout: true, MaxBytes: 2},
			messages: []*backend.LogMessage{msg("stdout", "abc")},
			expected: "",
		},
		{
			name:     "only selected streams count toward the budget",
			config:   backend.ContainerLogsOptions{ShowStderr: true, MaxBytes: 2},
			messages: []*backend.LogMessage{msg("stdout", "aaa"), msg("stderr", "bb"), msg("stderr", "c")},
			expected: "bb",
		},
		{
			name:     "error records do not count toward the budget",
			config:   backend.ContainerLogsOptions{ShowStdout: true, MaxBytes: 2},
			messages: []*backend.LogMessage{{Err: io.EOF}, msg("stdout", "x"), msg("stdout", "y"), msg("stdout", "z")},
			expected: "Error grabbing logs: EOF\nxy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &responseRecorder{}
			logstream.Write(t.Context(), rec, feed(tc.messages...), &tc.config, false)
			assert.Equal(t, rec.buf.String(), tc.expected)
		})
	}
}

func TestWriteMaxBytesMuxed(t *testing.T) {
	// Non-TTY / multiplexed stream: the 8-byte frame headers must not count
	// toward the budget, and timestamps/details must not count either.
	lines := []*backend.LogMessage{msg("stdout", "aa"), msg("stderr", "cc"), msg("stdout", "bb")}
	// Bodies: 2 (stdout) + 2 (stderr) = 4; the final 2-byte stdout record
	// would total 6 and must not be returned.
	cfg := backend.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Details:    true,
		MaxBytes:   4,
	}
	lines[0].Attrs = []backend.LogAttr{{Key: "k", Value: "vvvvvvvvvv"}}

	rec := &responseRecorder{}
	logstream.Write(t.Context(), rec, feed(lines...), &cfg, true)

	// Framed response is much larger than the 4-byte budget because headers,
	// timestamps, and details are not counted.
	assert.Assert(t, rec.buf.Len() > 4)

	var stdout, stderr bytes.Buffer
	_, err := stdcopy.StdCopy(&stdout, &stderr, &rec.buf)
	assert.NilError(t, err)
	assert.Assert(t, bytes.Contains(stdout.Bytes(), []byte("aa")), "stdout %q should contain the first record body", stdout.String())
	// Timestamps are prepended to every record, so only compare the body suffix.
	assert.Assert(t, bytes.HasSuffix(stderr.Bytes(), []byte("cc")), "stderr %q should end with the record body", stderr.String())
	assert.Assert(t, !bytes.Contains(stdout.Bytes(), []byte("bb")), "stdout %q should not contain the record past the budget", stdout.String())
}

func TestWriteMaxBytesFollowEndsResponse(t *testing.T) {
	// Emulate follow: the producer blocks because the channel is unbuffered
	// and records keep coming. Reaching the budget must return from Write
	// without consuming the record that exceeded the budget.
	msgs := make(chan *backend.LogMessage)
	stop := make(chan struct{})
	go func() {
		for _, m := range []*backend.LogMessage{msg("stdout", "a"), msg("stdout", "bb"), msg("stdout", "ccc")} {
			select {
			case msgs <- m:
			case <-stop:
				return
			}
		}
		<-stop // stay blocked, as a live follower would
	}()

	cfg := &backend.ContainerLogsOptions{ShowStdout: true, Follow: true, MaxBytes: 3}
	rec := &responseRecorder{}
	logstream.Write(t.Context(), rec, msgs, cfg, false)

	assert.Equal(t, rec.buf.String(), "abb")
	close(stop)
}

type jsonLine struct {
	Line   string `json:"Line"`
	Source string `json:"Source"`
}

func TestWriteJSONMaxBytes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		config       backend.ContainerLogsOptions
		messages     []*backend.LogMessage
		expectedLine []string
	}{
		{
			name:         "stops before the record that would exceed",
			config:       backend.ContainerLogsOptions{ShowStdout: true, MaxBytes: 3},
			messages:     []*backend.LogMessage{msg("stdout", "a"), msg("stdout", "bb"), msg("stdout", "ccc")},
			expectedLine: []string{"a", "bb"},
		},
		{
			name:         "zero means unlimited",
			config:       backend.ContainerLogsOptions{ShowStdout: true},
			messages:     []*backend.LogMessage{msg("stdout", "a"), msg("stdout", "bb")},
			expectedLine: []string{"a", "bb"},
		},
		{
			name:         "does not skip non-fitting records",
			config:       backend.ContainerLogsOptions{ShowStdout: true, MaxBytes: 3},
			messages:     []*backend.LogMessage{msg("stdout", "a"), msg("stdout", "bbb"), msg("stdout", "c")},
			expectedLine: []string{"a"},
		},
		{
			name:         "only selected streams count",
			config:       backend.ContainerLogsOptions{ShowStderr: true, MaxBytes: 2},
			messages:     []*backend.LogMessage{msg("stdout", "aaa"), msg("stderr", "bb"), msg("stderr", "c")},
			expectedLine: []string{"bb"},
		},
		{
			name:         "first record too large writes nothing",
			config:       backend.ContainerLogsOptions{ShowStdout: true, MaxBytes: 1},
			messages:     []*backend.LogMessage{msg("stdout", "abc")},
			expectedLine: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &responseRecorder{}
			logstream.WriteJSON(t.Context(), rec, feed(tc.messages...), &tc.config)

			var got []string
			dec := json.NewDecoder(&rec.buf)
			for {
				var l jsonLine
				if err := dec.Decode(&l); err != nil {
					assert.Assert(t, err == io.EOF, "unexpected decode error: %v", err)
					break
				}
				got = append(got, l.Line)
			}
			assert.DeepEqual(t, got, tc.expectedLine)
		})
	}
}
