package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// WithSubAgentTabs opens a background tab for every sub-agent session started
// by transfer_task or run_background_agent and streams it there instead of
// inline in the delegating tab. Ignored in lean mode, which has no tabs.
func WithSubAgentTabs() Option {
	return func(m *appModel) {
		m.subAgentTabs = true
	}
}

// handleSubAgentTabOpened builds the hidden page of a new sub-agent tab up
// front so the sub-session's stream accumulates before the user opens it.
func (m *appModel) handleSubAgentTabOpened(tabID string) (tea.Model, tea.Cmd) {
	runner := m.supervisor.GetRunner(tabID)
	if runner == nil || runner.App == nil {
		return m, nil
	}
	tab := m.ensureTab(tabID)
	if tab.chatPage != nil {
		return m, nil
	}
	ss := service.NewSessionState(runner.App.Session())
	cp := chat.New(m.ar, m.ctx(), runner.App, ss, m.chatPageOpts()...)
	cp.Update(m.planSidebarData)
	cp.SetRoutingID(tabID)
	tab.chatPage = cp
	tab.sessionState = ss
	tab.editor = editor.New(m.history, append(m.editorOpts(), editor.WithReadOnly())...)
	// The placeholder session is empty, so Init loads nothing into the page.
	return m, tea.Batch(cp.Init(), m.refreshTabs())
}

// isSubAgentTab reports whether tabID views a delegated sub-session. Such
// tabs are never persisted to the tui state store.
func (m *appModel) isSubAgentTab(tabID string) bool {
	if m.supervisor == nil {
		return false
	}
	runner := m.supervisor.GetRunner(tabID)
	return runner != nil && runner.ParentID != ""
}

// rejectInSubAgentTab refuses commands that would act on a sub-agent tab's
// placeholder session (persisting, running or rewriting it) or steer the
// shared runtime from a read-only view. Returns nil when msg may proceed.
func (m *appModel) rejectInSubAgentTab(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case messages.ToggleSessionStarMsg:
		// The session browser stars a named session; only the current one is a placeholder.
		if msg.SessionID != "" {
			return nil
		}
	case messages.ClearSessionMsg, messages.SwitchAgentMsg, messages.BranchFromEditMsg,
		messages.ForkSessionMsg, messages.ToggleYoloMsg, messages.TogglePauseMsg,
		messages.CompactSessionMsg, messages.UndoSnapshotMsg, messages.ResetSnapshotMsg,
		messages.EvalSessionMsg, messages.SetSessionTitleMsg,
		messages.RegenerateTitleMsg, messages.DropAttachedFileMsg, messages.AttachFileMsg,
		messages.AgentCommandMsg, messages.StartShellMsg, messages.ShowMCPPromptInputMsg,
		messages.MCPPromptMsg:
	default:
		return nil
	}
	if m.supervisor == nil || !m.isSubAgentTab(m.supervisor.ActiveID()) {
		return nil
	}
	return notification.WarningCmd("Sub-agent tabs are read-only views. Run this from the delegating tab.")
}

// handOffSubAgentAttention moves a closing sub-agent tab's unanswered prompts,
// with any input typed into them, to its parent tab: the sub-session keeps
// running and still blocks on them. Later prompts take the same route (see
// supervisor.routeSubAgentMsg).
func (m *appModel) handOffSubAgentAttention(tabID, parentID string) tea.Cmd {
	tab, parent := m.tabs[tabID], m.ensureTab(parentID)
	if tab == nil || tab.state == nil || parent.state == nil {
		return nil
	}
	var events []tea.Msg
	dialogs := make(map[tea.Msg]dialog.Dialog)
	if tab == m.activeTab {
		for _, open := range m.dialogMgr.TakeBackgroundDialogs(func(tea.Msg) bool { return true }) {
			events = append(events, open.OriginatingEvent)
			dialogs[open.OriginatingEvent] = open.Model
		}
	}
	for event := tab.state.Consume(); event != nil; event = tab.state.Consume() {
		events = append(events, event)
		if model := tab.attentionDialogs[event]; model != nil {
			dialogs[event] = model
		}
	}
	var cmds []tea.Cmd
	for _, event := range events {
		cmds = append(cmds, m.applyTabEvent(parent, event))
		if model := dialogs[event]; model != nil {
			if parent.attentionDialogs == nil {
				parent.attentionDialogs = make(map[tea.Msg]dialog.Dialog)
			}
			parent.attentionDialogs[event] = model
		}
	}
	return tea.Batch(cmds...)
}

// closeSubAgentTabs closes the sub-agent tabs of a tab being closed: their
// sub-sessions die with the parent's runtime.
func (m *appModel) closeSubAgentTabs(parentID string) tea.Cmd {
	var cmds []tea.Cmd
	for _, id := range m.supervisor.SubAgentTabIDs(parentID) {
		_, cmd := m.handleCloseTab(id)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}
