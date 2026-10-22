package supervisor

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tabstate"
)

// subAgentTitleWords bounds how much of the task a sub-agent tab title shows.
const subAgentTitleWords = 6

// subAgentRoute says where one message of a tab's subscription goes.
type subAgentRoute struct {
	open   []*runtime.SubSessionStartedEvent // tabs to open before delivering msg
	target string                            // sub-agent tab ID; "" is the subscribing tab
	msg    tea.Msg                           // nil when nothing is delivered
}

// subAgentRouter attributes one tab subscription's events to sub-agent tabs
// by session ID only: the runtime tags or wraps (SubSessionStreamEvent) every
// event of an announced sub-session, so concurrent children never mix.
// Used only from the subscription goroutine.
type subAgentRouter struct {
	known map[string]bool
}

func newSubAgentRouter() *subAgentRouter {
	return &subAgentRouter{known: make(map[string]bool)}
}

func (r *subAgentRouter) route(msg tea.Msg) subAgentRoute {
	var route subAgentRoute
	for {
		switch ev := msg.(type) {
		case *runtime.SubSessionStartedEvent:
			if !r.known[ev.SessionID] {
				r.known[ev.SessionID] = true
				route.open = append(route.open, ev)
			}
			return subAgentRoute{open: route.open}
		case *runtime.SubSessionStreamEvent:
			if !r.known[ev.SessionID] {
				// A subscription started mid-task never saw the announcement.
				r.known[ev.SessionID] = true
				route.open = append(route.open, &runtime.SubSessionStartedEvent{SessionID: ev.SessionID, AgentContext: ev.AgentContext, Background: ev.Background})
			}
			route.target = ev.SessionID
			msg = ev.Event
			continue
		case *runtime.TokenUsageEvent:
			// Usage stays with the parent's sidebar and cost accounting.
			return subAgentRoute{open: route.open, msg: msg}
		}
		break
	}
	// Tagged events of non-delegation sessions (skills) stay with the
	// enclosing sub-agent, or the subscribing tab.
	if scoped, ok := msg.(runtime.SessionScoped); ok && r.known[scoped.GetSessionID()] {
		route.target = scoped.GetSessionID()
	}
	route.msg = msg
	return route
}

// IsAttention reports prompts the runtime blocks on until the user answers.
func IsAttention(msg tea.Msg) bool {
	switch msg.(type) {
	case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent:
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
func (s *Supervisor) routeSubAgentMsg(send func(tea.Msg), router *subAgentRouter, parentID string, scope *messages.RouteScope, msg tea.Msg) {
	route := router.route(msg)
	for _, open := range route.open {
		if s.AddSubAgentTab(parentID, open) {
			send(messages.SubAgentTabOpenedMsg{TabID: open.SessionID})
		}
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
		if !IsAttention(route.msg) {
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

// ClosedSubAgentParent returns the parent tab of a closed sub-agent tab, or "".
func (s *Supervisor) ClosedSubAgentParent(tabID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closedSubAgents[tabID]
}
