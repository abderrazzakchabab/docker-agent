package root

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/shellpath"
)

type onEventHook struct {
	eventType string
	command   string
}

func parseOnEventFlags(specs []string) ([]onEventHook, error) {
	hooks := make([]onEventHook, 0, len(specs))
	for _, s := range specs {
		i := strings.Index(s, "=")
		if i < 1 {
			return nil, fmt.Errorf("--on-event expects <event-type>=<command>, got %q", s)
		}
		hooks = append(hooks, onEventHook{eventType: s[:i], command: s[i+1:]})
	}
	return hooks, nil
}

// withEventHooks returns an app.Opt that runs each configured shell command
// for matching events on the App fan-out. The event JSON is piped to the
// command's stdin. Commands run asynchronously and their failures are logged
// but never block the event bus.
func withEventHooks(hooks []onEventHook) app.Opt {
	if len(hooks) == 0 {
		return nil
	}
	return func(a *app.App) {
		//rubocop:disable Lint/ContextConnectivity
		go a.SubscribeWith(context.Background(), func(msg tea.Msg) {
			ev, ok := hookEvent(msg)
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				return
			}
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &head); err != nil {
				return
			}
			for _, h := range hooks {
				if h.eventType != "*" && h.eventType != head.Type {
					continue
				}
				go runEventHook(h.command, data)
			}
		})
	}
}

// maxHookOutput caps the diagnostic output we keep for a failed on-event
// hook. Large enough to be useful, small enough that a chatty or runaway
// command can't push unbounded data into the agent's heap.
const maxHookOutput = 4 * 1024

func runEventHook(command string, payload []byte) {
	shell, argsPrefix := shellpath.DetectShell()
	// Hooks are detached from the app context on purpose: a hook still
	// flushing the last event when the user exits the TUI should be allowed
	// to finish. Each invocation receives a single event on stdin, processes
	// it, and exits; the spawning goroutine ends with the subprocess.
	//rubocop:disable Lint/ContextConnectivity
	cmd := exec.CommandContext(context.Background(), shell, append(argsPrefix, command)...)
	cmd.Stdin = bytes.NewReader(payload)
	var out boundedWriter
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		slog.Warn("on-event hook failed", "command", command, "error", err, "output", strings.TrimSpace(out.String()))
	}
}

// boundedWriter captures up to maxHookOutput bytes from a hook subprocess
// and silently discards the rest. It satisfies io.Writer so it can be
// assigned to exec.Cmd's Stdout/Stderr.
//
// exec.Cmd spawns separate copy goroutines for Stdout and Stderr, so the
// underlying buffer must be safe for concurrent writes; that's what
// [concurrent.Buffer] gives us. The cap is enforced softly: between
// Len() and Write() another goroutine may slip in a chunk, so we may
// over-shoot maxHookOutput by at most one Write per concurrent stream
// (a few KB) — acceptable for diagnostic output.
type boundedWriter struct {
	buf concurrent.Buffer
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if remaining := maxHookOutput - b.buf.Len(); remaining > 0 {
		chunk := p
		if len(chunk) > remaining {
			chunk = chunk[:remaining]
		}
		_, _ = b.buf.Write(chunk)
	}
	return len(p), nil
}

func (b *boundedWriter) String() string {
	return b.buf.String()
}

// hookEvent returns the runtime event a hook sees for msg. It keeps hooks
// independent of --sub-agent-tabs: transfer_task events were always seen
// bare, background sub-session streams never.
func hookEvent(msg tea.Msg) (runtime.Event, bool) {
	switch ev := msg.(type) {
	case *runtime.SubSessionStartedEvent:
		return nil, false
	case *runtime.SubSessionStreamEvent:
		if ev.Background {
			return nil, false
		}
		return runtime.InnermostEvent(ev), true
	case runtime.Event:
		return ev, true
	default:
		return nil, false
	}
}
