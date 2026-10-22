package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// errSubSessionView rejects work a sub-session view must never do: it only
// watches a sub-session another App runs and answers its prompts.
var errSubSessionView = errors.New("sub-agent views are read-only")

// subSessionEventSource is an optional runtime capability: announcing
// delegated sub-sessions and attributing their events.
type subSessionEventSource interface {
	EnableSubSessionEvents()
}

// EnableSubSessionEvents opts the runtime into sub-session announcements
// (see [runtime.LocalRuntime.EnableSubSessionEvents]) and the fan-out into
// never dropping or evicting prompts. No-op for runtimes without the
// capability, such as remote ones.
func (a *App) EnableSubSessionEvents() {
	if src, ok := a.runtime.(subSessionEventSource); ok {
		src.EnableSubSessionEvents()
		a.subSessionEvents.Store(true)
	}
}

// NewSubSessionView returns a read-only App bound to an empty placeholder for
// the sub-session sessionID. Dialog answers (tool confirmations, max
// iterations, elicitations) resume a's runtime; everything that would run,
// reconfigure or persist through the view fails instead, so the placeholder
// can never overwrite the real sub-session in the store. The view is never
// started, so it registers no runtime handlers and emits no events.
func (a *App) NewSubSessionView(sessionID, title string) *App {
	opts := []session.Opt{session.WithID(sessionID), session.WithTitle(title)}
	if a.session != nil {
		opts = append(opts, session.WithWorkingDir(a.session.WorkingDir))
	}
	return New(a.ctx(), viewRuntime{a.runtime}, session.New(opts...), WithReadOnly())
}

// viewRuntime exposes a shared runtime's reads and prompt answers only.
type viewRuntime struct {
	runtime.Runtime
}

func (v viewRuntime) SetCurrentAgent(context.Context, string) error { return errSubSessionView }

func (v viewRuntime) RestartToolset(context.Context, string) error { return errSubSessionView }

func (v viewRuntime) ResetStartupInfo() {}

func (v viewRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (v viewRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, errSubSessionView
}

func (v viewRuntime) SessionStore() session.Store {
	store := v.Runtime.SessionStore()
	if store == nil {
		return nil
	}
	return readOnlyStore{store}
}

func (v viewRuntime) Summarize(context.Context, *session.Session, string, runtime.EventSink) {}

func (v viewRuntime) RunSkillFork(context.Context, *session.Session, skills.RunSkillArgs, runtime.EventSink) (*tools.ToolCallResult, error) {
	return nil, errSubSessionView
}

func (v viewRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", errSubSessionView
}

func (v viewRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return errSubSessionView
}

func (v viewRuntime) Steer(context.Context, runtime.QueuedMessage) error { return errSubSessionView }

func (v viewRuntime) FollowUp(context.Context, runtime.QueuedMessage) error { return errSubSessionView }

func (v viewRuntime) SetAgentModel(context.Context, string, string) error { return errSubSessionView }

func (v viewRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	return "", errSubSessionView
}

func (v viewRuntime) SetAgentThinkingLevel(context.Context, string, effort.Level) (effort.Level, error) {
	return "", errSubSessionView
}

// The shared runtime keeps a single handler per hook; a view must not steal them.
func (v viewRuntime) OnToolsChanged(func(runtime.Event)) {}

func (v viewRuntime) OnBackgroundEvent(func(runtime.Event)) {}

func (v viewRuntime) OnElicitationRequest(func(runtime.Event)) {}

func (v viewRuntime) TogglePause(context.Context) (bool, error) { return false, errSubSessionView }

// Close must not shut down the runtime the view borrows.
func (v viewRuntime) Close() error { return nil }

// ResolveGeneratedFile keeps generated media renderable in the view.
func (v viewRuntime) ResolveGeneratedFile(ctx context.Context, ref runtime.GeneratedFileRef) (*runtime.ResolvedGeneratedFile, error) {
	if resolver, ok := v.Runtime.(generatedFileResolver); ok {
		return resolver.ResolveGeneratedFile(ctx, ref)
	}
	return nil, fmt.Errorf("generated file resolution: %w", runtime.ErrUnsupported)
}

// readOnlyStore serves a session store's reads and refuses every write.
type readOnlyStore struct {
	session.Store
}

func (readOnlyStore) AddSession(context.Context, *session.Session) error { return errSubSessionView }

func (readOnlyStore) DeleteSession(context.Context, string) error { return errSubSessionView }

func (readOnlyStore) UpdateSession(context.Context, *session.Session) error { return errSubSessionView }

func (readOnlyStore) SetSessionStarred(context.Context, string, bool) error { return errSubSessionView }

func (readOnlyStore) AddMessage(context.Context, string, *session.Message) (int64, error) {
	return 0, errSubSessionView
}

func (readOnlyStore) UpdateMessage(context.Context, string, int64, *session.Message) error {
	return errSubSessionView
}

func (readOnlyStore) AddSubSession(context.Context, string, *session.Session) error {
	return errSubSessionView
}

func (readOnlyStore) PersistCompaction(context.Context, *session.Session, int64, int64, session.Item) error {
	return errSubSessionView
}

func (readOnlyStore) AddSummary(context.Context, string, session.Item) error {
	return errSubSessionView
}

func (readOnlyStore) AddError(context.Context, string, *session.Error) error {
	return errSubSessionView
}

func (readOnlyStore) UpdateSessionTokens(context.Context, string, int64, int64, float64) error {
	return errSubSessionView
}

func (readOnlyStore) UpdateSessionTitle(context.Context, string, string) error {
	return errSubSessionView
}

// Close must not close the store the view borrows.
func (readOnlyStore) Close() error { return nil }
