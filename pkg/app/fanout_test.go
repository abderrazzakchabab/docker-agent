package app

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

// TestFanOut_TurnBoundaryEventEvictsPendingDelta verifies that when a
// subscriber's buffer is full, a turn-boundary event (which SSE consumers
// cannot recover if lost) evicts the oldest pending message instead of being
// dropped.
func TestFanOut_TurnBoundaryEventEvictsPendingDelta(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		events := make(chan tea.Msg, 16)
		app := &App{
			ctx:              func() context.Context { return ctx },
			events:           events,
			throttleDuration: time.Millisecond,
		}

		// A one-slot subscriber makes the overflow deterministic. The subscriber
		// never reads, standing in for a consumer that fell behind.
		ch := make(chan tea.Msg, 1)
		app.addSubscriber(&eventSubscriber{ch: ch})
		app.fanoutOnce.Do(app.startFanOut)

		// Fill the subscriber's buffer with a droppable event.
		filler := runtime.NewTokenUsageEvent("sess", "root", &runtime.Usage{})
		events <- filler
		synctest.Wait()
		require.Len(t, ch, 1)

		// The turn-boundary event must displace the pending filler.
		stopped := runtime.StreamStopped("sess", "root", "normal")
		events <- stopped

		synctest.Wait()
		select {
		case msg := <-ch:
			assert.Equal(t, stopped, msg, "the stream_stopped event must survive the overflow")
		default:
			t.Fatal("the stream_stopped event must survive the overflow")
		}
	})
}

// TestFanOut_DroppableEventIsDroppedOnOverflow verifies the pre-existing
// behavior for non-boundary events: on overflow they are dropped, never
// evicting anything.
func TestFanOut_DroppableEventIsDroppedOnOverflow(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		events := make(chan tea.Msg, 16)
		app := &App{
			ctx:              func() context.Context { return ctx },
			events:           events,
			throttleDuration: time.Millisecond,
		}

		ch := make(chan tea.Msg, 1)
		app.addSubscriber(&eventSubscriber{ch: ch})
		// The witness is registered after ch, so once a message reaches it the
		// fan-out has already made its keep-or-drop decision for ch.
		witness := make(chan tea.Msg, 16)
		app.addSubscriber(&eventSubscriber{ch: witness})
		app.fanoutOnce.Do(app.startFanOut)

		first := runtime.NewTokenUsageEvent("sess", "root", &runtime.Usage{})
		events <- first
		synctest.Wait()
		require.Len(t, ch, 1)

		// A second droppable event overflows and is dropped; the first stays.
		second := runtime.NewTokenUsageEvent("sess", "other", &runtime.Usage{})
		events <- second
		synctest.Wait()
		require.Len(t, witness, 2)

		msg := <-ch
		assert.Equal(t, first, msg)
	})
}

// TestIsTurnBoundaryEvent_LooksInsideSubSessionWrappers verifies that
// sub-session boundaries and prompts survive overflow even when the runtime
// wrapped them for attribution, while wrapped deltas stay droppable.
func TestIsTurnBoundaryEvent_LooksInsideSubSessionWrappers(t *testing.T) {
	t.Parallel()

	wrap := func(ev runtime.Event) runtime.Event { return runtime.SubSessionStream("child", "ship", false, ev) }
	bg := func(ev runtime.Event) runtime.Event { return runtime.SubSessionStream("bg", "scout", true, ev) }
	protected := map[string]tea.Msg{
		"announcement":         runtime.SubSessionStarted("root", "child", "ship", "task", false),
		"wrapped stop":         wrap(runtime.StreamStopped("child", "ship", "normal")),
		"wrapped confirmation": wrap(&runtime.ToolCallConfirmationEvent{}),
		"wrapped max iter":     wrap(&runtime.MaxIterationsReachedEvent{}),
		"wrapped elicitation":  wrap(&runtime.ElicitationRequestEvent{}),
		"nested stop":          bg(wrap(runtime.StreamStopped("child", "ship", "normal"))),
		"nested announcement":  bg(runtime.SubSessionStarted("bg", "child", "ship", "", false)),
	}
	for name, msg := range protected {
		assert.True(t, isTurnBoundaryEvent(msg), name)
	}
	assert.False(t, isTurnBoundaryEvent(wrap(&runtime.AgentChoiceEvent{})), "wrapped deltas stay droppable")
	assert.False(t, isTurnBoundaryEvent(bg(&runtime.ToolCallOutputEvent{})))
	assert.False(t, isTurnBoundaryEvent(&runtime.ToolCallConfirmationEvent{}), "unwrapped events keep their existing policy")
}
