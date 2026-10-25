package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// transfer_task children stream AgentChoiceEvents bare, tagged only by
// SessionID: throttling must keep that attribution and never merge two
// concurrent sub-sessions of the same agent.
func TestMergeEventsKeepsSubSessionAttribution(t *testing.T) {
	t.Parallel()

	merged := (&App{}).mergeEvents([]tea.Msg{
		runtime.AgentChoice("ship", "child-A", "hello "),
		runtime.AgentChoice("ship", "child-A", "world"),
		runtime.AgentChoice("ship", "child-B", "BBB"),
		runtime.AgentChoiceReasoning("ship", "child-A", "think "),
		runtime.AgentChoiceReasoning("ship", "child-A", "more"),
		runtime.AgentChoiceReasoning("ship", "child-B", "other"),
	})
	require.Len(t, merged, 4)
	first := merged[0].(*runtime.AgentChoiceEvent)
	assert.Equal(t, "hello world", first.Content)
	assert.Equal(t, "child-A", first.SessionID)
	assert.Equal(t, "child-B", merged[1].(*runtime.AgentChoiceEvent).SessionID)
	reasoning := merged[2].(*runtime.AgentChoiceReasoningEvent)
	assert.Equal(t, "think more", reasoning.Content)
	assert.Equal(t, "child-A", reasoning.SessionID)
	assert.Equal(t, "child-B", merged[3].(*runtime.AgentChoiceReasoningEvent).SessionID)
}

func TestThrottledDeltaKeepsSubSessionID(t *testing.T) {
	t.Parallel()

	events := make(chan tea.Msg, 16)
	a := &App{ctx: func() context.Context { return t.Context() }, events: events, throttleDuration: 20 * time.Millisecond}
	ch := make(chan tea.Msg, 16)
	a.addSubscriber(&eventSubscriber{ch: ch})
	a.fanoutOnce.Do(a.startFanOut)

	events <- runtime.AgentChoice("ship", "child-A", "hello ")
	events <- runtime.AgentChoice("ship", "child-A", "world")
	select {
	case msg := <-ch:
		ev := msg.(*runtime.AgentChoiceEvent)
		assert.Equal(t, "hello world", ev.Content)
		assert.Equal(t, "child-A", ev.SessionID)
	case <-time.After(2 * time.Second):
		t.Fatal("nothing delivered")
	}
}

// pressureApp returns an App with sub-session events enabled whose only
// subscriber has a one-slot buffer, plus a witness that sees every delivery
// decision after it was made for the slow subscriber.
func pressureApp(t *testing.T) (events, slow, witness chan tea.Msg) {
	t.Helper()
	events = make(chan tea.Msg, 16)
	a := &App{ctx: func() context.Context { return t.Context() }, events: events, throttleDuration: time.Millisecond}
	a.subSessionEvents.Store(true)
	slow = make(chan tea.Msg, 1)
	a.addSubscriber(&eventSubscriber{ch: slow})
	witness = make(chan tea.Msg, 16)
	a.addSubscriber(&eventSubscriber{ch: witness})
	a.fanoutOnce.Do(a.startFanOut)
	return events, slow, witness
}

func confirmation() runtime.Event {
	return runtime.ToolCallConfirmation(tools.ToolCall{ID: "x"}, tools.Tool{Name: "shell"}, "ship", nil)
}

func TestFanOutUnderPressure_PromptIsNeverEvictedByABoundary(t *testing.T) {
	t.Parallel()
	events, slow, witness := pressureApp(t)

	prompt := runtime.SubSessionStream("child-A", "ship", false, confirmation())
	events <- prompt
	<-witness
	events <- runtime.StreamStarted("child-B", "scout")
	<-witness

	assert.Same(t, prompt, <-slow, "a sibling's stream start must not evict a pending prompt")
}

func TestFanOutUnderPressure_BarePromptEvictsADelta(t *testing.T) {
	t.Parallel()
	events, slow, witness := pressureApp(t)

	events <- runtime.AgentChoice("ship", "child-A", "filler")
	<-witness
	elicit := &runtime.ElicitationRequestEvent{Type: "elicitation_request", SessionID: "child-A"}
	events <- elicit
	<-witness

	assert.Same(t, elicit, <-slow, "a bare sub-session prompt must survive overflow")
}

func TestFanOutUnderPressure_PromptWaitsWhenOnlyPromptsArePending(t *testing.T) {
	t.Parallel()
	events, slow, witness := pressureApp(t)

	first := runtime.SubSessionStream("child-A", "ship", false, confirmation())
	events <- first
	<-witness
	second := &runtime.MaxIterationsReachedEvent{Type: "max_iterations_reached"}
	events <- second
	boundary := runtime.StreamStopped("root", "root", "normal")
	events <- boundary

	assert.Same(t, first, <-slow)
	assert.Same(t, second, <-slow, "the second prompt waits for room instead of being dropped")
	assert.Same(t, second, <-witness)
	select {
	case msg := <-witness:
		assert.Same(t, boundary, msg, "the fan-out keeps going once the prompt is delivered")
	case <-time.After(2 * time.Second):
		t.Fatal("the fan-out must keep delivering after a blocked prompt")
	}
}

func TestFanOutUnderPressure_AbandonedSubscriberDoesNotWedgeTheFanOut(t *testing.T) {
	t.Parallel()
	events := make(chan tea.Msg, 16)
	a := &App{ctx: func() context.Context { return t.Context() }, events: events, throttleDuration: time.Millisecond}
	a.subSessionEvents.Store(true)
	gone := make(chan tea.Msg, 1)
	goneSub := &eventSubscriber{ch: gone}
	a.addSubscriber(goneSub)
	witness := make(chan tea.Msg, 16)
	a.addSubscriber(&eventSubscriber{ch: witness})
	a.fanoutOnce.Do(a.startFanOut)

	events <- confirmation()
	<-witness
	blocked := confirmation()
	events <- blocked
	a.removeSubscriber(goneSub)
	select {
	case msg := <-witness:
		assert.Same(t, blocked, msg)
	case <-time.After(2 * time.Second):
		t.Fatal("a prompt for an unsubscribed channel must not block the fan-out")
	}
}

func TestFanOutUnderPressure_BoundaryNeverEvictsAPrompt(t *testing.T) {
	t.Parallel()
	events, slow, witness := pressureApp(t)

	prompt := confirmation()
	events <- prompt
	<-witness
	events <- runtime.StreamStarted("child-B", "scout")
	<-witness
	events <- runtime.AgentChoice("ship", "child-B", "after")
	<-witness

	assert.Same(t, prompt, <-slow, "with only prompts pending, a boundary is dropped instead")
}

// A read-only sub-session view must never write the store: its placeholder
// carries the real sub-session's ID.
func TestSubSessionViewCannotWriteTheStore(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := session.NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)

	parent := session.New(session.WithID("parent"), session.WithTitle("parent"))
	require.NoError(t, store.AddSession(t.Context(), parent))
	child := session.New(session.WithID("child"), session.WithTitle("Transferred task"), session.WithParentID("parent"))
	child.InputTokens = 1234
	require.NoError(t, store.AddSession(t.Context(), child))

	a := New(t.Context(), &mockRuntime{store: store}, parent)
	view := a.NewSubSessionView("child", "ship: fix the bug")
	require.True(t, view.IsReadOnly())

	assert.Error(t, view.SetCurrentAgentModel(t.Context(), "openai/gpt-4o"))
	assert.Error(t, view.UpdateSessionTitle(t.Context(), "renamed"))
	viewStore := view.SessionStore()
	assert.Error(t, viewStore.UpdateSession(t.Context(), view.Session()))
	assert.Error(t, viewStore.DeleteSession(t.Context(), "child"))
	assert.Error(t, viewStore.SetSessionStarred(t.Context(), "child", true))
	require.NoError(t, viewStore.Close(), "closing the view's store must not close the shared one")

	got, err := viewStore.GetSession(t.Context(), "child")
	require.NoError(t, err, "reads still work")
	assert.Equal(t, "parent", got.ParentID)
	assert.Equal(t, "Transferred task", got.Title)
	assert.Equal(t, int64(1234), got.InputTokens)
	assert.False(t, got.Starred)
}
