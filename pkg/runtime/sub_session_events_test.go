package runtime

import (
	"slices"
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
		if wrapped, ok := event.(*BackgroundSubSessionEvent); ok {
			assert.Equal(t, child.ID, wrapped.SessionID)
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
