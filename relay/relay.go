// Package relay exposes a backend that has no native A2A support as an A2A
// server. The official a2a-go SDK does all protocol work (transports, task
// store, streaming, push notifications); this package only adds caller
// authentication and the bridge from A2A executions to a Backend.
package relay

import (
	"context"
	"crypto/subtle"
	"errors"
	"iter"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Request is one unit of work handed to a backend.
type Request struct {
	TaskID    string `json:"task_id"`
	ContextID string `json:"context_id"`
	Caller    string `json:"caller"`
	Text      string `json:"text"`
	// Resumed is true when the caller answers an earlier input-required reply.
	Resumed bool `json:"resumed"`
}

// Reply states are the only outcomes a backend may report.
const (
	ReplyCompleted     = "completed"
	ReplyInputRequired = "input-required"
	ReplyRejected      = "rejected"
	ReplyFailed        = "failed"
)

// Reply is the backend's answer to a Request.
type Reply struct {
	State string `json:"state"`
	Text  string `json:"text"`
}

// Backend does the actual work. Handle blocks until the work is done, the
// context ends, or the backend gives up. It calls claimed once the agent
// behind it has actually picked the request up, so the relay reports
// "working" only when that is true.
type Backend interface {
	Handle(ctx context.Context, req Request, claimed func()) (Reply, error)
}

// Echo is a backend that answers immediately with the request text.
type Echo struct{}

// Handle implements Backend.
func (Echo) Handle(_ context.Context, req Request, claimed func()) (Reply, error) {
	claimed()
	return Reply{State: ReplyCompleted, Text: req.Text}, nil
}

// Executor adapts a Backend to the SDK's AgentExecutor.
type Executor struct {
	Backend Backend
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// Cancel implements a2asrv.AgentExecutor.
func (*Executor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

type outcome struct {
	reply Reply
	err   error
}

// Execute implements a2asrv.AgentExecutor.
func (e *Executor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// A new task starts as submitted; a resumed one (answer to
		// input-required) goes back to submitted until the backend claims it.
		first := a2a.Event(a2a.NewSubmittedTask(ec, ec.Message))
		if ec.StoredTask != nil {
			first = a2a.NewStatusUpdateEvent(ec, a2a.TaskStateSubmitted, nil)
		}
		if !yield(first, nil) {
			return
		}
		req := Request{
			TaskID:    string(ec.TaskID),
			ContextID: ec.ContextID,
			Text:      messageText(ec.Message),
			Resumed:   ec.StoredTask != nil,
		}
		if ec.User != nil {
			req.Caller = ec.User.Name
		}

		claimed := make(chan struct{}, 1)
		done := make(chan outcome, 1)
		go func() {
			r, err := e.Backend.Handle(ctx, req, func() {
				select {
				case claimed <- struct{}{}:
				default:
				}
			})
			done <- outcome{r, err}
		}()

		for {
			select {
			case <-claimed:
				if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
					return
				}
			case o := <-done:
				yieldOutcome(ec, o, yield)
				return
			}
		}
	}
}

func yieldOutcome(ec *a2asrv.ExecutorContext, o outcome, yield func(a2a.Event, error) bool) {
	msg := func(text string) *a2a.Message {
		return a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(text))
	}
	if o.err != nil {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed, msg(o.err.Error())), nil)
		return
	}
	switch o.reply.State {
	case ReplyCompleted:
		if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart(o.reply.Text)), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	case ReplyInputRequired:
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, msg(o.reply.Text)), nil)
	case ReplyRejected:
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateRejected, msg(o.reply.Text)), nil)
	default:
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed, msg(o.reply.Text)), nil)
	}
}

func messageText(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	var parts []string
	for _, p := range m.Parts {
		if t := p.Text(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// ErrNoCallers is returned when a relay would start with no way to authenticate anyone.
var ErrNoCallers = errors.New("no callers configured")

// CallerAuth is an SDK call interceptor that admits only requests carrying a
// known bearer token and records the caller's name as the task owner, so the
// SDK task store keeps each caller's tasks private.
type CallerAuth struct {
	a2asrv.PassthroughCallInterceptor
	tokens map[string]string // token -> caller name
}

// NewCallerAuth builds the interceptor from a "name=token,name=token" spec.
func NewCallerAuth(spec string) (*CallerAuth, error) {
	tokens := map[string]string{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, token, ok := strings.Cut(pair, "=")
		if !ok || name == "" || token == "" {
			return nil, errors.New("caller entries must look like name=token")
		}
		tokens[token] = name
	}
	if len(tokens) == 0 {
		return nil, ErrNoCallers
	}
	return &CallerAuth{tokens: tokens}, nil
}

// Before implements a2asrv.CallInterceptor.
func (a *CallerAuth) Before(ctx context.Context, cc *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	name, ok := a.lookup(cc)
	if !ok {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	cc.User = a2asrv.NewAuthenticatedUser(name, nil)
	return ctx, nil, nil
}

func (a *CallerAuth) lookup(cc *a2asrv.CallContext) (string, bool) {
	vals, _ := cc.ServiceParams().Get("Authorization")
	for _, v := range vals {
		token, ok := strings.CutPrefix(v, "Bearer ")
		if !ok {
			continue
		}
		for known, name := range a.tokens {
			if subtle.ConstantTimeCompare([]byte(token), []byte(known)) == 1 {
				return name, true
			}
		}
	}
	return "", false
}
