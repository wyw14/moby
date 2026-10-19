package container

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/logger/jsonfilelog"
	"github.com/moby/moby/v2/daemon/logger/local"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/termtest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// Regression test for #35370
// Makes sure that when following we don't get an EOF error when there are no logs
func TestLogsFollowTailEmpty(t *testing.T) {
	// FIXME(vdemeester) fails on a e2e run on linux...
	skip.If(t, testEnv.IsRemoteDaemon)
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	id := container.Run(ctx, t, apiClient, container.WithCmd("sleep", "100000"))

	logs, err := apiClient.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, Tail: "2"})
	assert.Check(t, err)
	defer logs.Close()

	_, err = stdcopy.StdCopy(io.Discard, io.Discard, logs)
	assert.Check(t, err)
}

func TestLogs(t *testing.T) {
	drivers := []string{local.Name, jsonfilelog.Name}

	for _, logDriver := range drivers {
		t.Run("driver "+logDriver, func(t *testing.T) {
			testLogs(t, logDriver)
		})
	}
}

func testLogs(t *testing.T, logDriver string) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	tests := []struct {
		desc        string
		logOps      client.ContainerLogsOptions
		expectedOut string
		expectedErr string
		tty         bool
	}{
		// TTY, only one output stream
		{
			desc: "tty/stdout and stderr",
			tty:  true,
			logOps: client.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: true,
			},
			expectedOut: "this is fineaccidents happen",
		},
		{
			desc: "tty/only stdout",
			tty:  true,
			logOps: client.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: false,
			},
			expectedOut: "this is fineaccidents happen",
		},
		{
			desc: "tty/only stderr",
			tty:  true,
			logOps: client.ContainerLogsOptions{
				ShowStdout: false,
				ShowStderr: true,
			},
			expectedOut: "",
		},
		// Without TTY, both stdout and stderr
		{
			desc: "without tty/stdout and stderr",
			tty:  false,
			logOps: client.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: true,
			},
			expectedOut: "this is fine",
			expectedErr: "accidents happen",
		},
		{
			desc: "without tty/only stdout",
			tty:  false,
			logOps: client.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: false,
			},
			expectedOut: "this is fine",
			expectedErr: "",
		},
		{
			desc: "without tty/only stderr",
			tty:  false,
			logOps: client.ContainerLogsOptions{
				ShowStdout: false,
				ShowStderr: true,
			},
			expectedOut: "",
			expectedErr: "accidents happen",
		},
	}

	pollTimeout := time.Second * 10
	if testEnv.DaemonInfo.OSType == "windows" {
		pollTimeout = StopContainerWindowsPollTimeout
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			tty := tc.tty
			id := container.Run(ctx, t, apiClient,
				container.WithCmd("sh", "-c", "echo -n this is fine; echo -n accidents happen >&2"),
				container.WithTty(tty),
				container.WithLogDriver(logDriver),
				// Logs do not need networking, and Windows HNS endpoint
				// setup is sensitive to these parallel short-lived starts.
				container.WithNetworkMode("none"),
			)
			defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

			poll.WaitOn(t, container.IsStopped(ctx, apiClient, id), poll.WithTimeout(pollTimeout))

			logs, err := apiClient.ContainerLogs(ctx, id, tc.logOps)
			assert.NilError(t, err)
			defer logs.Close()

			var stdout, stderr bytes.Buffer
			if tty {
				// TTY, only one output stream
				_, err = io.Copy(&stdout, logs)
			} else {
				_, err = stdcopy.StdCopy(&stdout, &stderr, logs)
			}
			assert.NilError(t, err)

			stdoutStr := stdout.String()

			if tty && testEnv.DaemonInfo.OSType == "windows" {
				stdoutStr = stripEscapeCodes(t, stdoutStr)

				// Special case for Windows Server 2019
				// Check only that the raw output stream contains strings
				// that were printed to container's stdout and stderr.
				// This is a workaround for the backspace being outputted in an unexpected place
				// which breaks the parsed output: https://github.com/moby/moby/issues/43710
				if strings.Contains(testEnv.DaemonInfo.OperatingSystem, "Windows Server Version 1809") {
					if tc.logOps.ShowStdout {
						assert.Check(t, is.Contains(stdout.String(), "this is fine"))
						assert.Check(t, is.Contains(stdout.String(), "accidents happen"))
					} else {
						assert.DeepEqual(t, stdoutStr, "")
					}
					return
				}
			}

			assert.DeepEqual(t, stdoutStr, tc.expectedOut)
			assert.DeepEqual(t, stderr.String(), tc.expectedErr)
		})
	}
}

// This hack strips the escape codes that appear in the Windows TTY output and don't have
// any effect on the text content.
// This doesn't handle all escape codes, only those that were encountered during testing.
func stripEscapeCodes(t *testing.T, input string) string {
	t.Logf("Stripping: %q\n", input)
	output, err := termtest.StripANSICommands(input)
	assert.NilError(t, err)
	return output
}

// TestLogsMaxBytes verifies the "max-bytes" response budget: after the
// existing stream/since/until/tail filters, only the raw content of selected
// records counts toward the budget, and a record that would exceed it ends
// the response without truncating the record or selecting later records.
func TestLogsMaxBytes(t *testing.T) {
	for _, logDriver := range []string{local.Name, jsonfilelog.Name} {
		t.Run("driver "+logDriver, func(t *testing.T) {
			t.Run("non-tty", func(t *testing.T) {
				t.Parallel()
				ctx := setupTest(t)
				apiClient := testEnv.APIClient()

				// Raw record bodies are 1, 2, and 3 bytes.
				id := container.Run(ctx, t, apiClient,
					container.WithCmd("sh", "-c", "printf 'a\\nbb\\nccc\\n'"),
					container.WithLogDriver(logDriver),
					container.WithNetworkMode("none"),
				)
				defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
				poll.WaitOn(t, container.IsStopped(ctx, apiClient, id))

				logs, err := apiClient.ContainerLogs(ctx, id, client.ContainerLogsOptions{
					ShowStdout: true,
					MaxBytes:   3,
				})
				assert.NilError(t, err)
				defer logs.Close()

				var stdout, stderr bytes.Buffer
				_, err = stdcopy.StdCopy(&stdout, &stderr, logs)
				assert.NilError(t, err)

				// "a" (1) and "bb" (2) fit exactly; "ccc" (3) would exceed the
				// budget and must not be returned or replaced by a later record.
				assert.Equal(t, stdout.String(), "abb")
				assert.Equal(t, stderr.String(), "")
			})

			t.Run("tty", func(t *testing.T) {
				// A TTY post-processes newlines into CRLF, changing record
				// sizes; only assert exact byte behavior on Linux.
				skip.If(t, testEnv.DaemonInfo.OSType == "windows")
				t.Parallel()
				ctx := setupTest(t)
				apiClient := testEnv.APIClient()

				id := container.Run(ctx, t, apiClient,
					container.WithCmd("sh", "-c", "printf 'a\\nbb\\n'"),
					container.WithTty(true),
					container.WithLogDriver(logDriver),
					container.WithNetworkMode("none"),
				)
				defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
				poll.WaitOn(t, container.IsStopped(ctx, apiClient, id))

				logs, err := apiClient.ContainerLogs(ctx, id, client.ContainerLogsOptions{
					ShowStdout: true,
					ShowStderr: true,
					// TTY records are "a\r" (2) and "bb\r" (3).
					MaxBytes: 2,
				})
				assert.NilError(t, err)
				defer logs.Close()

				out, err := io.ReadAll(logs)
				assert.NilError(t, err)
				assert.Equal(t, string(out), "a\r")
			})

			t.Run("negative value rejected", func(t *testing.T) {
				t.Parallel()
				ctx := setupTest(t)
				apiClient := testEnv.APIClient()

				id := container.Run(ctx, t, apiClient,
					container.WithCmd("sh", "-c", "printf 'a\\n'"),
					container.WithLogDriver(logDriver),
					container.WithNetworkMode("none"),
				)
				defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
				poll.WaitOn(t, container.IsStopped(ctx, apiClient, id))

				_, err := apiClient.ContainerLogs(ctx, id, client.ContainerLogsOptions{
					ShowStdout: true,
					MaxBytes:   -1,
				})
				assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
			})
		})
	}
}

// TestLogsFollowMaxBytes verifies that reaching the budget ends a following
// response (the client sees EOF) and releases the underlying log reader.
func TestLogsFollowMaxBytes(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	for _, logDriver := range []string{local.Name, jsonfilelog.Name} {
		t.Run("driver "+logDriver, func(t *testing.T) {
			id := container.Run(ctx, t, apiClient,
				container.WithCmd("sh", "-c", "printf 'a\\nbb\\n'; sleep 30"),
				container.WithLogDriver(logDriver),
				container.WithNetworkMode("none"),
			)
			defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
			poll.WaitOn(t, container.RunningStateFlagIs(ctx, apiClient, id, true))

			logs, err := apiClient.ContainerLogs(ctx, id, client.ContainerLogsOptions{
				ShowStdout: true,
				Follow:     true,
				MaxBytes:   1,
			})
			assert.NilError(t, err)
			defer logs.Close()

			type copyResult struct {
				out string
				err error
			}
			done := make(chan copyResult, 1)
			go func() {
				var stdout bytes.Buffer
				_, err := stdcopy.StdCopy(&stdout, io.Discard, logs)
				done <- copyResult{out: stdout.String(), err: err}
			}()

			// The response must end on its own once the budget is reached;
			// without the budget the follower would block until the container
			// is removed.
			poll.WaitOn(t, func(log poll.LogT) poll.Result {
				select {
				case <-done:
					return poll.Success()
				default:
					return poll.Continue("waiting for follow response to end at the byte budget")
				}
			}, poll.WithTimeout(15*time.Second))

			r := <-done
			assert.NilError(t, r.err)
			assert.Equal(t, r.out, "a")
		})
	}
}
