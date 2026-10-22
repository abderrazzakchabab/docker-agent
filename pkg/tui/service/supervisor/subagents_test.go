package supervisor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type routeStep struct {
	msg    tea.Msg
	target string // "" = parent tab
	open   string // sub-agent tab expected to open
	drop   bool   // nothing delivered
}

func runRouteSteps(t *testing.T, r *subAgentRouter, steps []routeStep) {
	t.Helper()
	for i, step := range steps {
		got := r.route(step.msg, "root")
		if step.open != "" {
			require.NotNil(t, got.open, "step %d (%T): expected tab %q to open", i, step.msg, step.open)
			assert.Equal(t, step.open, got.open.SessionID, "step %d", i)
		} else {
			assert.Nil(t, got.open, "step %d (%T): unexpected tab open", i, step.msg)
		}
		if step.drop {
			assert.Nil(t, got.msg, "step %d (%T): expected no delivery", i, step.msg)
			continue
		}
		require.NotNil(t, got.msg, "step %d (%T)", i, step.msg)
		assert.Equal(t, step.target, got.target, "step %d (%T)", i, step.msg)
	}
}

func TestSubAgentRouter_TransferTaskStreamsToChildTab(t *testing.T) {
	t.Parallel()

	confirm := &runtime.ToolCallConfirmationEvent{}
	runRouteSteps(t, newSubAgentRouter(), []routeStep{
		{msg: runtime.StreamStarted("root", "firstmate")},
		{msg: &runtime.ToolCallEvent{}},
		{msg: runtime.SubSessionStarted("root", "ship", "ship", "fix the bug", false), open: "ship", drop: true},
		{msg: runtime.StreamStarted("ship", "ship"), target: "ship"},
		{msg: &runtime.AgentChoiceEvent{SessionID: "ship"}, target: "ship"},
		{msg: confirm, target: "ship"},
		{msg: &runtime.ToolCallResponseEvent{}, target: "ship"},
		{msg: &runtime.TokenUsageEvent{SessionID: "ship"}},
		{msg: &runtime.ElicitationRequestEvent{SessionID: "ship"}, target: "ship"},
		{msg: runtime.StreamStopped("ship", "ship", "normal"), target: "ship"},
		{msg: runtime.SubSessionCompleted("root", session.New(session.WithID("ship")), "firstmate")},
		{msg: &runtime.ToolCallResponseEvent{}},
		{msg: runtime.StreamStopped("root", "firstmate", "normal")},
	})
}

func TestSubAgentRouter_NestedTransfersUseInnermostTab(t *testing.T) {
	t.Parallel()

	runRouteSteps(t, newSubAgentRouter(), []routeStep{
		{msg: runtime.SubSessionStarted("root", "ship", "ship", "", false), open: "ship", drop: true},
		{msg: runtime.StreamStarted("ship", "ship"), target: "ship"},
		{msg: runtime.SubSessionStarted("ship", "scout", "scout", "", false), open: "scout", drop: true},
		{msg: runtime.StreamStarted("scout", "scout"), target: "scout"},
		{msg: &runtime.ToolCallEvent{}, target: "scout"},
		{msg: runtime.StreamStopped("scout", "scout", "normal"), target: "scout"},
		{msg: &runtime.ToolCallResponseEvent{}, target: "ship"},
		// A skill sub-session is not a delegation: it renders inline in its owner's tab.
		{msg: runtime.StreamStarted("skill", "ship"), target: "ship"},
		{msg: runtime.StreamStopped("skill", "ship", "normal"), target: "ship"},
		{msg: runtime.StreamStopped("ship", "ship", "normal"), target: "ship"},
		{msg: &runtime.ToolCallEvent{}},
	})
}

func TestSubAgentRouter_RootBoundaryClearsStaleChildren(t *testing.T) {
	t.Parallel()

	runRouteSteps(t, newSubAgentRouter(), []routeStep{
		{msg: runtime.SubSessionStarted("root", "ship", "ship", "", false), open: "ship", drop: true},
		{msg: &runtime.ToolCallEvent{}, target: "ship"},
		{msg: runtime.StreamStopped("root", "firstmate", "canceled")},
		{msg: &runtime.ToolCallEvent{}},
	})
}

func TestSubAgentRouter_BackgroundAgentUnwrapsIntoItsTab(t *testing.T) {
	t.Parallel()

	inner := &runtime.ToolCallEvent{}
	r := newSubAgentRouter()
	runRouteSteps(t, r, []routeStep{
		{msg: runtime.SubSessionStarted("root", "bg", "scout", "survey", true), open: "bg", drop: true},
		// The foreground keeps streaming in the parent while the background agent runs.
		{msg: &runtime.ToolCallEvent{}},
		{msg: runtime.BackgroundSubSession("bg", "scout", runtime.StreamStarted("bg", "scout")), target: "bg"},
		{msg: runtime.BackgroundSubSession("bg", "scout", inner), target: "bg"},
		{msg: runtime.BackgroundSubSession("bg", "scout", runtime.SubSessionStarted("bg", "nested", "ship", "", false)), drop: true},
		{msg: &runtime.ElicitationRequestEvent{SessionID: "bg"}, target: "bg"},
		{msg: &runtime.TokenUsageEvent{SessionID: "bg"}},
		{msg: &runtime.AgentChoiceEvent{SessionID: "root"}},
		// Late subscribers open the tab on the first wrapped event.
		{msg: runtime.BackgroundSubSession("late", "ship", inner), open: "late", target: "late"},
		{msg: runtime.BackgroundSubSession("late", "ship", inner), target: "late"},
	})
	assert.Same(t, inner, r.route(runtime.BackgroundSubSession("bg", "scout", inner), "root").msg, "wrapped events are delivered unwrapped")
}

func TestSubAgentRouter_AppNoticesStayWithParent(t *testing.T) {
	t.Parallel()

	runRouteSteps(t, newSubAgentRouter(), []routeStep{
		{msg: runtime.SubSessionStarted("root", "ship", "ship", "", false), open: "ship", drop: true},
		// Concurrent app-level notices are not part of the child's stream.
		{msg: messages.StreamCancelledMsg{}},
		{msg: &runtime.ToolsetInfoEvent{}},
		{msg: &runtime.MCPInitStartedEvent{}},
		{msg: &runtime.RAGIndexingStartedEvent{}},
		{msg: &runtime.PlanChangedEvent{}},
		{msg: &runtime.ShellOutputEvent{}},
		{msg: &runtime.TeamInfoEvent{}},
		// Untagged events of the child's own run loop follow it.
		{msg: &runtime.AgentInfoEvent{}, target: "ship"},
		{msg: &runtime.WarningEvent{}, target: "ship"},
		{msg: &runtime.MaxIterationsReachedEvent{}, target: "ship"},
		{msg: &runtime.ToolCallOutputEvent{}, target: "ship"},
	})
}

func TestSubAgentTabTitle(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "ship", subAgentTabTitle("ship", "  "))
	assert.Equal(t, "ship: fix the bug", subAgentTabTitle("ship", "fix the\nbug"))
	assert.Equal(t, "scout: one two three four five six …", subAgentTabTitle("scout", "one two three four five six seven"))
}

func TestRouteSubAgentMsg_OpensTabNextToParentAndRoutes(t *testing.T) {
	t.Parallel()

	s := newTestSupervisor([]string{"A", "B"}, "A")
	s.subAgentTabs = true
	parent := s.runners["A"]
	parent.Scope = &messages.RouteScope{}
	router := newSubAgentRouter()
	var sent []tea.Msg
	send := func(msg tea.Msg) { sent = append(sent, msg) }

	s.routeSubAgentMsg(send, router, "A", parent, parent.Scope, runtime.SubSessionStarted("A", "ship", "ship", "fix it", false))
	s.routeSubAgentMsg(send, router, "A", parent, parent.Scope, runtime.StreamStarted("ship", "ship"))
	s.routeSubAgentMsg(send, router, "A", parent, parent.Scope, runtime.SubSessionStarted("A", "bg", "scout", "", true))

	assert.Equal(t, []string{"A", "ship", "bg", "B"}, s.order, "sub-agent tabs group after their parent")
	assert.Equal(t, "A", s.activeID, "opening a sub-agent tab must not steal focus")
	child := s.runners["ship"]
	require.NotNil(t, child)
	assert.Equal(t, "A", child.ParentID)
	assert.Equal(t, "ship", child.State.SessionID())
	assert.Equal(t, []string{"ship", "bg"}, s.SubAgentTabIDs("A"))
	assert.Empty(t, s.SubAgentTabIDs(""))

	require.Len(t, sent, 3)
	assert.Equal(t, messages.SubAgentTabOpenedMsg{TabID: "ship"}, sent[0])
	routed, ok := sent[1].(messages.RoutedMsg)
	require.True(t, ok)
	assert.Equal(t, "ship", routed.SessionID)
	assert.Same(t, child.Scope, routed.Scope)
	assert.Equal(t, messages.SubAgentTabOpenedMsg{TabID: "bg"}, sent[2])

	tabs, _ := s.GetTabs()
	require.Len(t, tabs, 4)
	assert.Equal(t, "ship: fix it", tabs[1].Title)
	assert.False(t, tabs[1].IsActive)
}

func TestRouteSubAgentMsg_ClosedTabStillSurfacesPrompts(t *testing.T) {
	t.Parallel()

	s := newTestSupervisor([]string{"A"}, "A")
	parent := s.runners["A"]
	scope := &messages.RouteScope{}
	router := newSubAgentRouter()
	var sent []tea.Msg
	send := func(msg tea.Msg) { sent = append(sent, msg) }
	route := func(msg tea.Msg) { s.routeSubAgentMsg(send, router, "A", parent, scope, msg) }

	route(runtime.SubSessionStarted("A", "ship", "ship", "", false))
	route(runtime.SubSessionStarted("A", "bg", "scout", "", true))
	route(runtime.StreamStarted("ship", "ship"))
	assert.Equal(t, "A", s.CloseSession("ship"))
	assert.Equal(t, "A", s.CloseSession("bg"))
	sent = nil

	// Display-only events of a hidden sub-agent are dropped, not leaked into the parent.
	route(&runtime.ToolCallEvent{})
	route(&runtime.AgentChoiceEvent{SessionID: "ship"})
	route(runtime.BackgroundSubSession("bg", "scout", &runtime.ToolCallEvent{}))
	assert.Empty(t, sent)

	// The runtime blocks on these until answered: they must still reach the user.
	confirm := &runtime.ToolCallConfirmationEvent{}
	maxIter := &runtime.MaxIterationsReachedEvent{}
	fgElicit := &runtime.ElicitationRequestEvent{SessionID: "ship"}
	bgElicit := &runtime.ElicitationRequestEvent{SessionID: "bg"}
	for _, prompt := range []tea.Msg{confirm, maxIter, fgElicit, bgElicit} {
		route(prompt)
	}
	require.Len(t, sent, 4)
	for i, prompt := range []tea.Msg{confirm, maxIter, fgElicit, bgElicit} {
		routed, ok := sent[i].(messages.RoutedMsg)
		require.True(t, ok)
		assert.Equal(t, "A", routed.SessionID, "prompt %T goes to the parent tab", prompt)
		assert.Same(t, scope, routed.Scope)
		assert.Same(t, prompt, routed.Inner)
	}

	sent = nil
	route(runtime.StreamStopped("ship", "ship", "normal"))
	route(&runtime.ToolCallEvent{})
	require.Len(t, sent, 1, "the parent's own events resume once the child stops")
	assert.Equal(t, "A", sent[0].(messages.RoutedMsg).SessionID)
	assert.Equal(t, []string{"A"}, s.order, "closed sub-agent tabs are not reopened")
}

func TestAddSubAgentTab_RejectsUnknownOrNestedParents(t *testing.T) {
	t.Parallel()

	s := newTestSupervisor([]string{"A"}, "A")
	assert.False(t, s.AddSubAgentTab("missing", &runtime.SubSessionStartedEvent{SessionID: "x"}))
	assert.False(t, s.AddSubAgentTab("A", &runtime.SubSessionStartedEvent{}))
	require.True(t, s.AddSubAgentTab("A", &runtime.SubSessionStartedEvent{SessionID: "ship"}))
	assert.False(t, s.AddSubAgentTab("A", &runtime.SubSessionStartedEvent{SessionID: "ship"}), "no duplicates")
	assert.False(t, s.AddSubAgentTab("ship", &runtime.SubSessionStartedEvent{SessionID: "scout"}), "sub-agent tabs group under the subscribing tab")
}
