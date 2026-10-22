package root

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestParseOnEventFlags(t *testing.T) {
	t.Parallel()

	hooks, err := parseOnEventFlags([]string{
		"stream_stopped=say done",
		"*=tee /tmp/events.log",
	})
	require.NoError(t, err)
	require.Len(t, hooks, 2)
	assert.Equal(t, "stream_stopped", hooks[0].eventType)
	assert.Equal(t, "say done", hooks[0].command)
	assert.Equal(t, "*", hooks[1].eventType)
	assert.Equal(t, "tee /tmp/events.log", hooks[1].command)
}

func TestParseOnEventFlags_BadFormat(t *testing.T) {
	t.Parallel()

	cases := []string{"no-equals", "=missing-type"}
	for _, s := range cases {
		_, err := parseOnEventFlags([]string{s})
		assert.Error(t, err, "expected error for %q", s)
	}
}

func TestBoundedWriter_CapsAtMaxHookOutput(t *testing.T) {
	t.Parallel()

	var b boundedWriter

	n, err := b.Write(bytes.Repeat([]byte("a"), maxHookOutput-3))
	require.NoError(t, err)
	assert.Equal(t, maxHookOutput-3, n)

	// A write that straddles the cap is fully accepted from the caller's
	// perspective (so io.Copy doesn't error) but only the bytes up to the
	// cap are retained.
	n, err = b.Write([]byte("bbbbbb"))
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	// Subsequent writes past the cap are silently discarded.
	n, err = b.Write([]byte("ccccc"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	got := b.String()
	assert.Len(t, got, maxHookOutput)
	assert.True(t, strings.HasPrefix(got, strings.Repeat("a", maxHookOutput-3)))
	assert.True(t, strings.HasSuffix(got, "bbb"))
}

func TestHookEventIgnoresSubAgentTabWrapping(t *testing.T) {
	t.Parallel()

	toolCall := &runtime.ToolCallEvent{}
	ev, ok := hookEvent(runtime.SubSessionStream("child", "ship", false, toolCall))
	require.True(t, ok)
	assert.Same(t, toolCall, ev, "transfer_task events reach hooks bare, as without --sub-agent-tabs")

	_, ok = hookEvent(runtime.SubSessionStream("bg", "scout", true, toolCall))
	assert.False(t, ok, "background streams never reached hooks")
	_, ok = hookEvent(runtime.SubSessionStarted("root", "child", "ship", "", false))
	assert.False(t, ok)

	stop := runtime.StreamStopped("root", "root", "normal")
	ev, ok = hookEvent(stop)
	require.True(t, ok)
	assert.Same(t, stop, ev)
}
