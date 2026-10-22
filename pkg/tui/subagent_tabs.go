package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
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
	runner := m.supervisor.GetRunner(tabID)
	return runner != nil && runner.ParentID != ""
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
