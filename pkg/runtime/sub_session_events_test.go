package runtime

import (
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

func newSubSessionEventsRuntime(t *testing.T) *LocalRuntime {
	t.Helper()
	workerStream := newStreamBuilder().AddContent("worker done").AddStopWithUsage(10, 5).Build()
	prov := &mockProvider{id: "test/mock-model", stream: workerStream}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	agent.WithSubAgents(worker)(root)

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}

func TestTransferTask_AnnouncesSubSessionOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	for _, enabled := range []bool{false, true} {
		rt := newSubSessionEventsRuntime(t)
		if enabled {
			rt.EnableSubSessionEvents()
		}
		parent := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
		evts := make(chan Event, 128)
		_, err := rt.handleTaskTransfer(t.Context(), parent, transferToolCall("worker"), NewChannelSink(evts), tools.NopRuntime{})
		require.NoError(t, err)
		close(evts)

		var started *SubSessionStartedEvent
		var childStreamBeforeAnnounce bool
		for event := range evts {
			switch ev := event.(type) {
			case *SubSessionStartedEvent:
				started = ev
			case *StreamStartedEvent:
				childStreamBeforeAnnounce = childStreamBeforeAnnounce || started == nil
			}
		}
		if !enabled {
			assert.Nil(t, started, "no announcement without opt-in")
			continue
		}
		require.NotNil(t, started)
		assert.False(t, childStreamBeforeAnnounce, "announcement must precede the child's stream")
		assert.Equal(t, parent.ID, started.ParentSessionID)
		assert.Equal(t, firstSubSession(parent).ID, started.SessionID)
		assert.Equal(t, "worker", started.AgentName)
		assert.Equal(t, "do work", started.Task)
		assert.False(t, started.Background)
	}
}

func TestRunAgent_ForwardsWrappedStreamWhenEnabled(t *testing.T) {
	t.Parallel()

	rt := newSubSessionEventsRuntime(t)
	rt.EnableSubSessionEvents()
	var forwarded []Event
	rt.OnBackgroundEvent(func(event Event) { forwarded = append(forwarded, event) })

	parent := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	res := rt.RunAgent(t.Context(), agenttool.RunParams{AgentName: "worker", Task: "background work", ParentSession: parent})
	require.Empty(t, res.ErrMsg)
	child := firstSubSession(parent)
	require.NotNil(t, child)

	require.NotEmpty(t, forwarded)
	started, ok := forwarded[0].(*SubSessionStartedEvent)
	require.True(t, ok, "first forwarded event must announce the sub-session, got %T", forwarded[0])
	assert.Equal(t, child.ID, started.SessionID)
	assert.True(t, started.Background)
	assert.Equal(t, "background work", started.Task)

	var inner []Event
	for _, event := range forwarded[1:] {
		if wrapped, ok := event.(*SubSessionStreamEvent); ok {
			assert.Equal(t, child.ID, wrapped.SessionID)
			assert.True(t, wrapped.Background)
			inner = append(inner, wrapped.Event)
		}
	}
	require.NotEmpty(t, inner)
	assert.True(t, slices.ContainsFunc(inner, func(e Event) bool {
		started, ok := e.(*StreamStartedEvent)
		return ok && started.SessionID == child.ID
	}), "the child's stream start is forwarded")
	assert.IsType(t, &StreamStoppedEvent{}, inner[len(inner)-1])
	for _, event := range inner {
		_, isUsage := event.(*TokenUsageEvent)
		assert.False(t, isUsage, "token usage stays unwrapped for the parent's accounting")
	}
}

// transfer_task calls of one model turn run concurrently and share the
// parent's sink: every untagged child event must be wrapped with its own
// child's session ID, and only when sub-session events are enabled.
func TestTransferTask_ConcurrentChildrenAreAttributedAtSource(t *testing.T) {
	t.Parallel()

	for _, enabled := range []bool{false, true} {
		a := probeAgent("a", "a done", func() {})
		b := probeAgent("b", "b done", func() {})
		root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
		agent.WithSubAgents(a, b)(root)
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, a, b)),
			WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		if enabled {
			rt.EnableSubSessionEvents()
		}

		// A pinned parent pins each child to its target: unpinned concurrent
		// children share the runtime's current agent, which would blur the
		// per-agent attribution this test checks.
		parent := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true), session.WithAgentName("root"))
		evts := make(chan Event, 512)
		sink := NewChannelSink(evts)
		results := make(map[string]string)
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, target := range []string{"a", "b"} {
			wg.Go(func() {
				res, err := rt.handleTaskTransfer(t.Context(), parent, transferToolCall(target), sink, tools.NopRuntime{})
				assert.NoError(t, err)
				mu.Lock()
				defer mu.Unlock()
				results[target] = res.Output
			})
		}
		wg.Wait()
		close(evts)
		assert.Equal(t, map[string]string{"a": "a done", "b": "b done"}, results, "both results reach the parent")

		childOf := make(map[string]string) // sub-session ID -> agent
		var wrapped, rawToolEvents int
		for event := range evts {
			switch ev := event.(type) {
			case *SubSessionStartedEvent:
				childOf[ev.SessionID] = ev.AgentName
			case *SubSessionStreamEvent:
				wrapped++
				require.Contains(t, childOf, ev.SessionID, "wrappers follow their announcement")
				assert.False(t, ev.Background)
				assert.Equal(t, childOf[ev.SessionID], ev.Event.GetAgentName(), "event attributed to its own child")
				_, tagged := ev.Event.(SessionScoped)
				assert.False(t, tagged && ev.Event.(SessionScoped).GetSessionID() != "", "tagged events are not wrapped")
			case *StreamStartedEvent:
				if enabled {
					assert.Equal(t, childOf[ev.SessionID], ev.AgentName, "tagged child events carry their own ID")
				}
			case *PartialToolCallEvent, *ToolCallEvent, *ToolCallResponseEvent:
				rawToolEvents++
			}
		}
		if !enabled {
			assert.Empty(t, childOf, "no announcements without opt-in")
			assert.Zero(t, wrapped, "flag off: child events stay unwrapped")
			assert.NotZero(t, rawToolEvents)
			continue
		}
		assert.Len(t, childOf, 2)
		assert.NotZero(t, wrapped)
		assert.Zero(t, rawToolEvents, "no child tool event reaches the parent stream unattributed")
	}
}
