package supervisor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type routeStep struct {
	msg    tea.Msg
	target string   // "" = parent tab
	open   []string // sub-agent tabs expected to open
	drop   bool     // nothing delivered
}

func runRouteSteps(t *testing.T, r *subAgentRouter, steps []routeStep) {
	t.Helper()
	for i, step := range steps {
		got := r.route(step.msg)
		var opened []string
		for _, ev := range got.open {
			opened = append(opened, ev.SessionID)
		}
		assert.Equal(t, step.open, opened, "step %d (%T): opened tabs", i, step.msg)
		if step.drop {
			assert.Nil(t, got.msg, "step %d (%T): expected no delivery", i, step.msg)
			continue
		}
		require.NotNil(t, got.msg, "step %d (%T)", i, step.msg)
		assert.Equal(t, step.target, got.target, "step %d (%T)", i, step.msg)
	}
}

func wrap(id string, ev runtime.Event) runtime.Event {
	return runtime.SubSessionStream(id, id, false, ev)
}

func confirmation(agent string) *runtime.ToolCallConfirmationEvent {
	return runtime.ToolCallConfirmation(tools.ToolCall{ID: agent}, tools.Tool{Name: "shell"}, agent, nil).(*runtime.ToolCallConfirmationEvent)
}

func TestSubAgentRouter_TransferTaskStreamsToChildTab(t *testing.T) {
	t.Parallel()

	runRouteSteps(t, newSubAgentRouter(), []routeStep{
		{msg: runtime.StreamStarted("root", "firstmate")},
		{msg: &runtime.ToolCallEvent{}},
		{msg: runtime.SubSessionStarted("root", "ship", "ship", "fix the bug", false), open: []string{"ship"}, drop: true},
		{msg: runtime.StreamStarted("ship", "ship"), target: "ship"},
		{msg: &runtime.AgentChoiceEvent{SessionID: "ship"}, target: "ship"},
		{msg: wrap("ship", confirmation("ship")), target: "ship"},
		{msg: wrap("ship", &runtime.ToolCallResponseEvent{}), target: "ship"},
		{msg: &runtime.TokenUsageEvent{SessionID: "ship"}},
		{msg: &runtime.ElicitationRequestEvent{SessionID: "ship"}, target: "ship"},
		// A skill run by the child streams inline in the child's tab.
		{msg: runtime.StreamStarted("skill", "ship")},
		{msg: wrap("ship", runtime.StreamStarted("skill", "ship")), target: "ship"},
		{msg: runtime.StreamStopped("ship", "ship", "normal"), target: "ship"},
		{msg: runtime.SubSessionCompleted("root", session.New(session.WithID("ship")), "firstmate")},
		{msg: &runtime.ToolCallResponseEvent{}},
		{msg: runtime.StreamStopped("root", "firstmate", "normal")},
	})
}

// transfer_task calls of one model turn run concurrently: attribution must
// come from the event, never from arrival order.
func TestSubAgentRouter_ConcurrentTransferTasksStayApart(t *testing.T) {
	t.Parallel()

	confirmA, confirmB := confirmation("a"), confirmation("b")
	r := newSubAgentRouter()
	runRouteSteps(t, r, []routeStep{
		{msg: runtime.SubSessionStarted("root", "A", "a", "", false), open: []string{"A"}, drop: true},
		{msg: runtime.SubSessionStarted("root", "B", "b", "", false), open: []string{"B"}, drop: true},
		{msg: runtime.StreamStarted("A", "a"), target: "A"},
		{msg: runtime.StreamStarted("B", "b"), target: "B"},
		{msg: wrap("B", &runtime.ToolCallEvent{}), target: "B"},
		{msg: wrap("A", &runtime.ToolCallEvent{}), target: "A"},
		{msg: wrap("A", confirmA), target: "A"},
		{msg: runtime.StreamStopped("A", "a", "normal"), target: "A"},
		{msg: wrap("B", confirmB), target: "B"},
		{msg: wrap("B", &runtime.ToolCallResponseEvent{}), target: "B"},
		{msg: runtime.StreamStopped("B", "b", "normal"), target: "B"},
		// Both delegation results land in the parent.
		{msg: &runtime.ToolCallResponseEvent{}},
		{msg: &runtime.ToolCallResponseEvent{}},
	})
	assert.Same(t, confirmB, r.route(wrap("B", confirmB)).msg, "wrapped events are delivered unwrapped")
}

func TestSubAgentRouter_BackgroundAgentUnwrapsIntoItsTab(t *testing.T) {
	t.Parallel()

	inner := &runtime.ToolCallEvent{}
	bg := func(ev runtime.Event) runtime.Event { return runtime.SubSessionStream("bg", "scout", true, ev) }
	runRouteSteps(t, newSubAgentRouter(), []routeStep{
		{msg: runtime.SubSessionStarted("root", "bg", "scout", "survey", true), open: []string{"bg"}, drop: true},
		// The foreground keeps streaming in the parent while the background agent runs.
		{msg: &runtime.ToolCallEvent{}},
		{msg: bg(runtime.StreamStarted("bg", "scout")), target: "bg"},
		{msg: bg(inner), target: "bg"},
		{msg: &runtime.ElicitationRequestEvent{SessionID: "bg"}, target: "bg"},
		{msg: &runtime.TokenUsageEvent{SessionID: "bg"}},
		{msg: &runtime.AgentChoiceEvent{SessionID: "root"}},
		// A background agent's own transfer_task gets a tab too.
		{msg: bg(runtime.SubSessionStarted("bg", "nested", "ship", "", false)), open: []string{"nested"}, drop: true},
		{msg: bg(runtime.StreamStarted("nested", "ship")), target: "nested"},
		{msg: bg(wrap("nested", confirmation("ship"))), target: "nested"},
		// Late subscribers open the tab on the first wrapped event.
		{msg: runtime.SubSessionStream("late", "ship", true, inner), open: []string{"late"}, target: "late"},
		{msg: runtime.SubSessionStream("late", "ship", true, inner), target: "late"},
	})
}

func TestSubAgentRouter_UntaggedEventsStayWithParent(t *testing.T) {
	t.Parallel()

	runRouteSteps(t, newSubAgentRouter(), []routeStep{
		{msg: runtime.SubSessionStarted("root", "ship", "ship", "", false), open: []string{"ship"}, drop: true},
		{msg: runtime.StreamStarted("ship", "ship"), target: "ship"},
		// While a child streams, unwrapped events are app-level or the parent's own.
		{msg: messages.StreamCancelledMsg{}},
		{msg: &runtime.ToolsetInfoEvent{}},
		{msg: &runtime.MCPInitStartedEvent{}},
		{msg: &runtime.RAGIndexingStartedEvent{}},
		{msg: &runtime.PlanChangedEvent{}},
		{msg: &runtime.ShellOutputEvent{}},
		{msg: &runtime.AgentInfoEvent{}},
		{msg: &runtime.MaxIterationsReachedEvent{}},
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

	s.routeSubAgentMsg(send, router, "A", parent.Scope, runtime.SubSessionStarted("A", "ship", "ship", "fix it", false))
	s.routeSubAgentMsg(send, router, "A", parent.Scope, runtime.StreamStarted("ship", "ship"))
	s.routeSubAgentMsg(send, router, "A", parent.Scope, runtime.SubSessionStarted("A", "bg", "scout", "", true))

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
	scope := &messages.RouteScope{}
	router := newSubAgentRouter()
	var sent []tea.Msg
	send := func(msg tea.Msg) { sent = append(sent, msg) }
	route := func(msg tea.Msg) { s.routeSubAgentMsg(send, router, "A", scope, msg) }

	route(runtime.SubSessionStarted("A", "ship", "ship", "", false))
	route(runtime.SubSessionStarted("A", "bg", "scout", "", true))
	route(runtime.StreamStarted("ship", "ship"))
	assert.Equal(t, "A", s.CloseSession("ship"))
	assert.Equal(t, "A", s.CloseSession("bg"))
	assert.Equal(t, "A", s.ClosedSubAgentParent("ship"))
	assert.Empty(t, s.ClosedSubAgentParent("A"))
	sent = nil

	// Display-only events of a hidden sub-agent are dropped, not leaked into the parent.
	route(wrap("ship", &runtime.ToolCallEvent{}))
	route(&runtime.AgentChoiceEvent{SessionID: "ship"})
	route(runtime.SubSessionStream("bg", "scout", true, &runtime.ToolCallEvent{}))
	assert.Empty(t, sent)

	// The runtime blocks on these until answered: they must still reach the user.
	confirm := confirmation("ship")
	maxIter := &runtime.MaxIterationsReachedEvent{}
	fgElicit := &runtime.ElicitationRequestEvent{SessionID: "ship"}
	bgElicit := &runtime.ElicitationRequestEvent{SessionID: "bg"}
	route(wrap("ship", confirm))
	route(wrap("ship", maxIter))
	route(fgElicit)
	route(bgElicit)
	require.Len(t, sent, 4)
	for i, prompt := range []tea.Msg{confirm, maxIter, fgElicit, bgElicit} {
		routed, ok := sent[i].(messages.RoutedMsg)
		require.True(t, ok)
		assert.Equal(t, "A", routed.SessionID, "prompt %T goes to the parent tab", prompt)
		assert.Same(t, scope, routed.Scope)
		assert.Same(t, prompt, routed.Inner)
	}
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
