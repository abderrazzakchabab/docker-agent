package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
)

const subAgentTabID = "child-session"

// newSubAgentTabModel opens a sub-agent tab under the initial tab the way the
// supervisor's router does, then delivers the TUI notification.
func newSubAgentTabModel(t *testing.T) (m *appModel, parentID string) {
	t.Helper()
	m = newTabLifecycleModel(t)
	parentID = m.supervisor.ActiveID()
	ev := runtime.SubSessionStarted(parentID, subAgentTabID, "ship", "fix the bug", false).(*runtime.SubSessionStartedEvent)
	require.True(t, m.supervisor.AddSubAgentTab(parentID, ev))
	_, _ = m.Update(messages.SubAgentTabOpenedMsg{TabID: subAgentTabID})
	return m, parentID
}

func subAgentConfirmation() *runtime.ToolCallConfirmationEvent {
	return runtime.ToolCallConfirmation(tools.ToolCall{ID: "tool"}, tools.Tool{Name: "shell"}, "ship", nil).(*runtime.ToolCallConfirmationEvent)
}

func TestSubAgentTabOpensInBackground(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	parentID := m.supervisor.ActiveID()
	parentTab, parentApp := m.activeTab, m.application
	ev := runtime.SubSessionStarted(parentID, subAgentTabID, "ship", "fix the bug", false).(*runtime.SubSessionStartedEvent)
	require.True(t, m.supervisor.AddSubAgentTab(parentID, ev))

	_, _ = m.Update(messages.SubAgentTabOpenedMsg{TabID: subAgentTabID})

	assert.Equal(t, parentID, m.supervisor.ActiveID(), "opening a sub-agent tab must not steal focus")
	assert.Same(t, parentTab, m.activeTab)
	assert.Same(t, parentApp, m.application)
	tab := m.tabs[subAgentTabID]
	require.NotNil(t, tab)
	assert.NotNil(t, tab.chatPage, "the page exists before activation so the stream accumulates")
	assert.NotNil(t, tab.editor)
	runner := m.supervisor.GetRunner(subAgentTabID)
	require.NotNil(t, runner)
	assert.True(t, runner.App.IsReadOnly())
	assert.Equal(t, subAgentTabID, runner.App.Session().ID)
	assert.Empty(t, runner.App.Session().Messages, "the parent's conversation must not load into the sub-agent tab")
	tabs, active := m.supervisor.GetTabs()
	require.Len(t, tabs, 2)
	assert.Equal(t, 0, active)
	assert.Equal(t, "ship: fix the bug", tabs[1].Title)
}

func TestSubAgentTabPromptIsAnsweredFromItsTab(t *testing.T) {
	t.Parallel()
	m, _ := newSubAgentTabModel(t)
	confirm := subAgentConfirmation()

	_, _ = m.Update(queuedRuntimeDelivery(m, subAgentTabID, confirm))
	_, _, attention := m.tabs[subAgentTabID].state.Snapshot()
	assert.True(t, attention, "a hidden sub-agent prompt marks its tab")
	assert.False(t, m.dialogMgr.Open(), "the prompt must not open over the parent tab")

	_, _ = m.handleSwitchTab(subAgentTabID)
	opened := takeAttention(m)
	require.Len(t, opened, 1)
	assert.Same(t, confirm, opened[0].OriginatingEvent)
}

func TestClosingSubAgentTabHandsPromptsToParent(t *testing.T) {
	t.Parallel()
	for _, childActive := range []bool{false, true} {
		name := "hidden"
		if childActive {
			name = "visible"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, parentID := newSubAgentTabModel(t)
			confirm := subAgentConfirmation()
			_, _ = m.Update(queuedRuntimeDelivery(m, subAgentTabID, confirm))
			if childActive {
				_, _ = m.handleSwitchTab(subAgentTabID)
				require.True(t, m.dialogMgr.Open())
			}

			_, _ = m.handleCloseTab(subAgentTabID)

			assert.Nil(t, m.supervisor.GetRunner(subAgentTabID))
			assert.Equal(t, parentID, m.supervisor.ActiveID())
			opened := takeAttention(m)
			require.Len(t, opened, 1, "the sub-session still waits on the prompt, so the parent must show it")
			assert.Same(t, confirm, opened[0].OriginatingEvent)
		})
	}
}

func TestClosingParentClosesSubAgentTabs(t *testing.T) {
	t.Parallel()
	m, parentID := newSubAgentTabModel(t)
	_, _ = m.handleSpawnSession("/other")
	otherID := m.supervisor.ActiveID()
	_, _ = m.handleSwitchTab(parentID)

	_, _ = m.handleCloseTab(parentID)

	assert.Nil(t, m.supervisor.GetRunner(subAgentTabID))
	assert.NotContains(t, m.tabs, subAgentTabID)
	assert.Equal(t, 1, m.supervisor.Count())
	assert.Equal(t, otherID, m.supervisor.ActiveID())
}

func TestSubAgentTabRejectsSessionCommands(t *testing.T) {
	t.Parallel()
	m, parentID := newSubAgentTabModel(t)
	assert.Nil(t, m.rejectInSubAgentTab(messages.ClearSessionMsg{}), "normal tabs keep every command")

	_, _ = m.handleSwitchTab(subAgentTabID)
	assert.NotNil(t, m.rejectInSubAgentTab(messages.ClearSessionMsg{}))
	assert.NotNil(t, m.rejectInSubAgentTab(messages.ToggleSessionStarMsg{}))

	_, _ = m.Update(messages.ClearSessionMsg{})
	runner := m.supervisor.GetRunner(subAgentTabID)
	assert.Equal(t, subAgentTabID, runner.App.Session().ID, "/clear must not replace the viewed sub-session")
	assert.Equal(t, subAgentTabID, m.tabs[subAgentTabID].state.SessionID())
	assert.Equal(t, parentID, runner.ParentID)
}

// Not parallel: paths.SetDataDir is process-global.
func TestSubAgentTabsAreNeverPersisted(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })
	m, parentID := newSubAgentTabModel(t)
	store, err := tuistate.New(t.Context())
	require.NoError(t, err)
	m.tuiStore = store
	persistedID := m.persistedSessionID(parentID)
	require.NoError(t, store.AddTab(t.Context(), persistedID, "/initial"))
	require.NoError(t, store.SetActiveTab(t.Context(), persistedID))

	_, _ = m.handleSwitchTab(subAgentTabID)
	_, _ = m.Update(messages.ToggleSidebarMsg{})
	m.handleReorderTab(messages.ReorderTabMsg{FromIdx: 1, ToIdx: 0})
	_, _ = m.handleCloseTab(subAgentTabID)

	saved, active, err := store.GetTabs(t.Context())
	require.NoError(t, err)
	require.Len(t, saved, 1)
	assert.Equal(t, persistedID, saved[0].SessionID)
	assert.False(t, saved[0].SidebarCollapsed)
	assert.Equal(t, persistedID, active)
}

// The subscription routed a prompt to the sub-agent tab just before the tab
// closed: delivery must redirect it, since the sub-session waits on it.
func TestInFlightSubAgentPromptAfterCloseReachesParent(t *testing.T) {
	t.Parallel()
	m, parentID := newSubAgentTabModel(t)
	confirm := subAgentConfirmation()
	inflight := queuedRuntimeDelivery(m, subAgentTabID, confirm)

	_, _ = m.handleCloseTab(subAgentTabID)
	_, _ = m.Update(inflight)

	require.Equal(t, parentID, m.supervisor.ActiveID())
	opened := takeAttention(m)
	require.Len(t, opened, 1, "the in-flight prompt must reach the parent tab")
	assert.Same(t, confirm, opened[0].OriginatingEvent)

	_, _ = m.Update(queuedRuntimeDeliveryTo(subAgentTabID, &runtime.AgentChoiceEvent{SessionID: subAgentTabID}))
	assert.False(t, m.dialogMgr.Open(), "display events of a closed sub-agent tab are dropped")
}

func queuedRuntimeDeliveryTo(id string, event tea.Msg) messages.RoutedMsg {
	return messages.RoutedMsg{SessionID: id, Scope: &messages.RouteScope{}, Inner: event}
}

// Sub-agent tabs pass an allowlist only: the shared runtime and the store
// must be unreachable from the read-only view.
func TestSubAgentTabCommandAllowlist(t *testing.T) {
	t.Parallel()
	m, _ := newSubAgentTabModel(t)
	_, _ = m.handleSwitchTab(subAgentTabID)
	require.Same(t, m.supervisor.GetRunner(subAgentTabID).App, m.application)

	for _, msg := range []tea.Msg{
		messages.ChangeModelMsg{ModelRef: "openai/gpt-4o"},
		messages.SetThinkingLevelMsg{Level: "high"},
		messages.RestartToolsetMsg{},
		messages.ToggleHideToolResultsMsg{},
		messages.ToggleSplitDiffMsg{},
		messages.OpenModelPickerMsg{},
		messages.SendMsg{Content: "hi"},
		messages.ShowCostDialogMsg{},
		messages.DeleteSessionMsg{SessionID: "named"},
		&messages.CompactSessionMsg{},
	} {
		assert.NotNil(t, m.rejectInSubAgentTab(msg), "%T must be rejected in a sub-agent tab", msg)
	}
	for _, msg := range []tea.Msg{
		messages.SwitchTabMsg{}, messages.CloseTabMsg{}, messages.ReorderTabMsg{},
		messages.SpawnSessionMsg{}, messages.OpenSessionBrowserMsg{}, messages.LoadSessionMsg{},
		messages.ElicitationResponseMsg{}, messages.CopyLastResponseToClipboardMsg{},
		messages.ExitSessionMsg{}, messages.RoutedMsg{}, messages.TabsUpdatedMsg{},
		messages.SubAgentTabOpenedMsg{}, messages.StreamCancelledMsg{},
	} {
		assert.Nil(t, m.rejectInSubAgentTab(msg), "%T must stay available in a sub-agent tab", msg)
	}

	_, _ = m.handleSwitchTab(m.supervisor.GetRunner(subAgentTabID).ParentID)
	assert.Nil(t, m.rejectInSubAgentTab(messages.ChangeModelMsg{}), "normal tabs keep every command")
}
