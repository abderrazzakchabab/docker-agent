package supervisor

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tabstate"
)

// subAgentTitleWords bounds how much of the task a sub-agent tab title shows.
const subAgentTitleWords = 6

// subAgentRoute says where one message of a tab's subscription goes.
type subAgentRoute struct {
	open   *runtime.SubSessionStartedEvent // tab to open before delivering msg
	target string                          // sub-agent tab ID; "" is the subscribing tab
	msg    tea.Msg                         // nil when nothing is delivered
}

// subAgentRouter attributes one tab subscription's events to sub-agent tabs.
// transfer_task children stream inline on the parent's channel, so events
// without a session ID (tool calls, confirmations) belong to the innermost
// open child; background children arrive wrapped with their session ID.
// Used only from the subscription goroutine.
type subAgentRouter struct {
	known map[string]bool
	stack []string
}

func newSubAgentRouter() *subAgentRouter {
	return &subAgentRouter{known: make(map[string]bool)}
}

func (r *subAgentRouter) route(msg tea.Msg, rootID string) subAgentRoute {
	switch ev := msg.(type) {
	case *runtime.SubSessionStartedEvent:
		r.known[ev.SessionID] = true
		if !ev.Background {
			r.stack = append(r.stack, ev.SessionID)
		}
		return subAgentRoute{open: ev}
	case *runtime.BackgroundSubSessionEvent:
		var open *runtime.SubSessionStartedEvent
		if !r.known[ev.SessionID] {
			// A subscription started mid-task never saw the announcement.
			r.known[ev.SessionID] = true
			open = &runtime.SubSessionStartedEvent{SessionID: ev.SessionID, AgentContext: ev.AgentContext, Background: true}
		}
		// Nested delegations of a background agent render inline in its tab.
		if _, nested := ev.Event.(*runtime.SubSessionStartedEvent); nested {
			return subAgentRoute{open: open}
		}
		return subAgentRoute{open: open, target: ev.SessionID, msg: ev.Event}
	case *runtime.TokenUsageEvent:
		// Usage stays with the parent's sidebar and cost accounting.
		return subAgentRoute{msg: msg}
	case *runtime.SubSessionCompletedEvent:
		// Backstop for a child whose stream never reported its stop.
		if sub, ok := ev.SubSession.(*session.Session); ok {
			r.closeStream(sub.ID)
		}
		return subAgentRoute{msg: msg}
	}
	if _, ok := msg.(runtime.Event); !ok {
		return subAgentRoute{msg: msg}
	}
	if scoped, ok := msg.(runtime.SessionScoped); ok {
		switch id := scoped.GetSessionID(); {
		case id != "" && id == rootID:
			if isStreamBoundary(msg) {
				r.stack = nil
			}
			return subAgentRoute{msg: msg}
		case r.known[id]:
			switch msg.(type) {
			case *runtime.StreamStartedEvent:
				if n := len(r.stack); n == 0 || r.stack[n-1] != id {
					r.stack = append(r.stack, id)
				}
			case *runtime.StreamStoppedEvent:
				r.closeStream(id)
			}
			return subAgentRoute{target: id, msg: msg}
		}
	}
	if n := len(r.stack); n > 0 && inStream(msg) {
		return subAgentRoute{target: r.stack[n-1], msg: msg}
	}
	return subAgentRoute{msg: msg}
}

// inStream reports whether an event without a known session belongs to the
// stream it interrupts: tagged events of nested non-delegation sessions (skills)
// and the untagged types a run loop emits. App-level notices that can arrive
// concurrently (toolset changes, MCP init, RAG indexing, plans, shell output)
// stay with the subscribing tab.
func inStream(msg tea.Msg) bool {
	if scoped, ok := msg.(runtime.SessionScoped); ok && scoped.GetSessionID() != "" {
		return true
	}
	switch msg.(type) {
	case *runtime.PartialToolCallEvent, *runtime.ToolCallEvent, *runtime.ToolCallConfirmationEvent,
		*runtime.ToolCallOutputEvent, *runtime.ToolCallResponseEvent, *runtime.MaxIterationsReachedEvent,
		*runtime.WarningEvent, *runtime.ModelFallbackEvent, *runtime.AuthorizationEvent,
		*runtime.HookBlockedEvent, *runtime.AgentInfoEvent, *runtime.AgentSwitchingEvent:
		return true
	default:
		return false
	}
}

// isAttention reports prompts the runtime blocks on until the user answers.
func isAttention(msg tea.Msg) bool {
	switch msg.(type) {
	case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent:
		return true
	default:
		return false
	}
}

// closeStream pops id along with any children left open above it.
func (r *subAgentRouter) closeStream(id string) {
	if i := slices.Index(r.stack, id); i >= 0 {
		r.stack = r.stack[:i]
	}
}

func isStreamBoundary(msg tea.Msg) bool {
	switch msg.(type) {
	case *runtime.StreamStartedEvent, *runtime.StreamStoppedEvent:
		return true
	default:
		return false
	}
}

// subAgentTabTitle names a sub-agent tab after its agent and the start of its task.
func subAgentTabTitle(agentName, task string) string {
	words := strings.Fields(task)
	if len(words) == 0 {
		return agentName
	}
	if len(words) > subAgentTitleWords {
		words = append(words[:subAgentTitleWords:subAgentTitleWords], "…")
	}
	return agentName + ": " + strings.Join(words, " ")
}

// EnableSubAgentTabs makes sessions added afterwards open a background tab
// for every delegated sub-agent session and route its events there.
func (s *Supervisor) EnableSubAgentTabs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subAgentTabs = true
}

// routeSubAgentMsg delivers one message of parentID's subscription, opening a
// sub-agent tab first when the runtime announces a new sub-session.
func (s *Supervisor) routeSubAgentMsg(send func(tea.Msg), router *subAgentRouter, parentID string, parent *SessionRunner, scope *messages.RouteScope, msg tea.Msg) {
	route := router.route(msg, parent.State.SessionID())
	if route.open != nil && s.AddSubAgentTab(parentID, route.open) {
		send(messages.SubAgentTabOpenedMsg{TabID: route.open.SessionID})
	}
	if route.msg == nil {
		return
	}
	if route.target != "" {
		s.mu.RLock()
		child := s.runners[route.target]
		s.mu.RUnlock()
		if child != nil {
			send(messages.RoutedMsg{SessionID: route.target, Scope: child.Scope, Inner: route.msg})
			return
		}
		// A closed sub-agent tab only hid the view: its session keeps running
		// and may still block on a prompt, which then surfaces in the parent
		// tab exactly as it would without sub-agent tabs.
		if !isAttention(route.msg) {
			return
		}
	}
	send(messages.RoutedMsg{SessionID: parentID, Scope: scope, Inner: route.msg})
}

// AddSubAgentTab adds a tab viewing sub-session ev after parentID's tab and
// its earlier sub-agent tabs, without activating it. The tab shares the
// parent's runtime through a read-only view App.
func (s *Supervisor) AddSubAgentTab(parentID string, ev *runtime.SubSessionStartedEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	parent := s.runners[parentID]
	if ev.SessionID == "" || s.runners[ev.SessionID] != nil || parent == nil || parent.ParentID != "" {
		return false
	}
	title := subAgentTabTitle(ev.AgentName, ev.Task)
	var view *app.App
	if parent.App != nil {
		view = parent.App.NewSubSessionView(ev.SessionID, title)
	}
	s.runners[ev.SessionID] = &SessionRunner{
		ID:         ev.SessionID,
		App:        view,
		WorkingDir: parent.WorkingDir,
		State:      tabstate.New(ev.SessionID, title),
		Scope:      &messages.RouteScope{},
		ParentID:   parentID,
	}

	at := slices.Index(s.order, parentID) + 1
	for at < len(s.order) && s.runners[s.order[at]] != nil && s.runners[s.order[at]].ParentID == parentID {
		at++
	}
	s.order = slices.Insert(s.order, at, ev.SessionID)
	s.notifyTabsUpdated()
	return true
}

// SubAgentTabIDs returns the open sub-agent tabs of parentID in tab order.
func (s *Supervisor) SubAgentTabIDs(parentID string) []string {
	if parentID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for _, id := range s.order {
		if runner := s.runners[id]; runner != nil && runner.ParentID == parentID {
			ids = append(ids, id)
		}
	}
	return ids
}
