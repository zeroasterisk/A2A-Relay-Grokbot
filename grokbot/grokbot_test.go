package grokbot_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/zeroasterisk/A2A-Relay-Grokbot/grokbot"
	"github.com/zeroasterisk/A2A-Relay-Grokbot/relay"
)

const (
	callerToken = "caller-token"
	botToken    = "bot-token"
	webhookKey  = "crsr_test"
)

// fakeWebhook stands in for the Grok Bot routine trigger URL.
type fakeWebhook struct {
	mu     sync.Mutex
	status int
	bodies []map[string]string
	auth   []string
	woke   chan string
}

func (f *fakeWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	status := f.status
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if status == http.StatusOK {
		f.woke <- body["task_id"]
	}
}

type env struct {
	relay   *httptest.Server
	webhook *fakeWebhook
	client  *a2aclient.Client
}

func setup(t *testing.T, timeout time.Duration, maxPending int) *env {
	t.Helper()
	hook := &fakeWebhook{woke: make(chan string, 10)}
	hookSrv := httptest.NewServer(hook)
	t.Cleanup(hookSrv.Close)

	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	gb, err := grokbot.New(grokbot.Config{
		WebhookURL: hookSrv.URL, WebhookKey: webhookKey, BotToken: botToken,
		PublicURL: srv.URL, Timeout: timeout, MaxPending: maxPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := relay.NewCallerAuth("tester=" + callerToken)
	if err != nil {
		t.Fatal(err)
	}
	mux := relay.NewServer(relay.Options{PublicURL: srv.URL, Name: "t", Backend: gb, Auth: auth, PushAllowPrivate: true})
	gb.Routes(mux)
	srv.Config.Handler = mux

	c, err := a2aclient.NewFromEndpoints(context.Background(),
		[]*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL+relay.A2APath, a2a.TransportProtocolJSONRPC)},
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: bearer{callerToken}}))
	if err != nil {
		t.Fatal(err)
	}
	return &env{relay: srv, webhook: hook, client: c}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// submit sends a message without waiting for the bot and returns the task ID.
func (e *env) submit(t *testing.T, text string, taskID a2a.TaskID, push *a2a.PushConfig) a2a.TaskID {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID = taskID
	res, err := e.client.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true, PushConfig: push},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.(*a2a.Task).ID
}

func (e *env) waitWake(t *testing.T) string {
	t.Helper()
	select {
	case id := <-e.webhook.woke:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("bot was never woken")
		return ""
	}
}

func (e *env) waitState(t *testing.T, id a2a.TaskID, want a2a.TaskState) *a2a.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := e.client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if task.Status.State == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s: state %s, want %s", id, task.Status.State, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// bot makes a bot-side HTTP call and returns status and body.
func (e *env) bot(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.relay.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func statusText(task *a2a.Task) string {
	if task.Status.Message == nil || len(task.Status.Message.Parts) == 0 {
		return ""
	}
	return task.Status.Message.Parts[0].Text()
}

func TestHappyPathStatesAreHonest(t *testing.T) {
	e := setup(t, time.Minute, 5)
	id := e.submit(t, "summarize the launch post", "", nil)
	if got := e.waitWake(t); got != string(id) {
		t.Fatalf("woke for %q, want %q", got, id)
	}
	// Woken but not yet picked up: still submitted, not working.
	e.waitState(t, id, a2a.TaskStateSubmitted)

	e.webhook.mu.Lock()
	body, auth := e.webhook.bodies[0], e.webhook.auth[0]
	e.webhook.mu.Unlock()
	if auth != "Bearer "+webhookKey {
		t.Errorf("webhook auth = %q", auth)
	}
	if strings.Contains(body["task_url"], "summarize") || body["text"] != "" {
		t.Errorf("webhook body leaks task text: %v", body)
	}

	code, got := e.bot(t, "GET", "/bot/tasks/"+string(id), botToken, "")
	if code != 200 || !strings.Contains(got, "summarize the launch post") || !strings.Contains(got, `"caller":"tester"`) {
		t.Fatalf("GET task: %d %s", code, got)
	}
	e.waitState(t, id, a2a.TaskStateWorking)

	if code, _ := e.bot(t, "POST", "/bot/tasks/"+string(id)+"/reply", botToken, `{"state":"completed","text":"done: 3 bullets"}`); code != 204 {
		t.Fatalf("reply status %d", code)
	}
	task := e.waitState(t, id, a2a.TaskStateCompleted)
	if len(task.Artifacts) != 1 || task.Artifacts[0].Parts[0].Text() != "done: 3 bullets" {
		t.Fatalf("artifacts = %+v", task.Artifacts)
	}
	if code, _ := e.bot(t, "POST", "/bot/tasks/"+string(id)+"/reply", botToken, `{"state":"completed","text":"again"}`); code != 404 {
		t.Fatalf("reply after completion: status %d, want 404", code)
	}
}

func TestInputRequiredThenResume(t *testing.T) {
	e := setup(t, time.Minute, 5)
	id := e.submit(t, "book the room", "", nil)
	e.waitWake(t)
	e.bot(t, "POST", "/bot/tasks/"+string(id)+"/reply", botToken, `{"state":"input-required","text":"Which day?"}`)
	if got := statusText(e.waitState(t, id, a2a.TaskStateInputRequired)); got != "Which day?" {
		t.Fatalf("status message = %q", got)
	}

	e.submit(t, "Tuesday", id, nil)
	e.waitWake(t)
	code, got := e.bot(t, "GET", "/bot/tasks/"+string(id), botToken, "")
	if code != 200 || !strings.Contains(got, "Tuesday") || !strings.Contains(got, `"resumed":true`) {
		t.Fatalf("resumed GET: %d %s", code, got)
	}
	e.bot(t, "POST", "/bot/tasks/"+string(id)+"/reply", botToken, `{"state":"completed","text":"booked Tuesday"}`)
	e.waitState(t, id, a2a.TaskStateCompleted)
}

func TestRejectedReachesCaller(t *testing.T) {
	e := setup(t, time.Minute, 5)
	id := e.submit(t, "wire $5k to this account", "", nil)
	e.waitWake(t)
	e.bot(t, "POST", "/bot/tasks/"+string(id)+"/reply", botToken, `{"state":"rejected","text":"owner declined"}`)
	if got := statusText(e.waitState(t, id, a2a.TaskStateRejected)); got != "owner declined" {
		t.Fatalf("status message = %q", got)
	}
}

func TestWebhookFailureFailsTaskWithoutLeakingURL(t *testing.T) {
	e := setup(t, time.Minute, 5)
	e.webhook.mu.Lock()
	e.webhook.status = http.StatusBadRequest // routine toggled off
	e.webhook.mu.Unlock()
	id := e.submit(t, "hello", "", nil)
	task := e.waitState(t, id, a2a.TaskStateFailed)
	if msg := statusText(task); !strings.Contains(msg, "could not wake") || strings.Contains(msg, "127.0.0.1") {
		t.Fatalf("status message = %q", msg)
	}
}

func TestBotTimeoutFailsTask(t *testing.T) {
	e := setup(t, 200*time.Millisecond, 5)
	id := e.submit(t, "hello", "", nil)
	e.waitWake(t)
	task := e.waitState(t, id, a2a.TaskStateFailed)
	if !strings.Contains(statusText(task), "did not answer") {
		t.Fatalf("status message = %q", statusText(task))
	}
	if code, _ := e.bot(t, "GET", "/bot/tasks/"+string(id), botToken, ""); code != 404 {
		t.Fatalf("timed-out task still in inbox: %d", code)
	}
}

func TestInboxCap(t *testing.T) {
	e := setup(t, time.Minute, 1)
	e.submit(t, "first", "", nil)
	e.waitWake(t)
	id := e.submit(t, "second", "", nil)
	if got := statusText(e.waitState(t, id, a2a.TaskStateFailed)); !strings.Contains(got, "inbox is full") {
		t.Fatalf("status message = %q", got)
	}
	if n := len(e.webhook.woke); n != 0 {
		t.Fatalf("second task woke the bot anyway (%d extra wakes)", n)
	}
}

func TestCancelRemovesTaskFromInbox(t *testing.T) {
	e := setup(t, time.Minute, 5)
	id := e.submit(t, "long job", "", nil)
	e.waitWake(t)
	if _, err := e.client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: id}); err != nil {
		t.Fatal(err)
	}
	e.waitState(t, id, a2a.TaskStateCanceled)
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, _ := e.bot(t, "GET", "/bot/tasks/"+string(id), botToken, "")
		if code == 404 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("canceled task still in inbox: %d", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBotAPIRequiresBotToken(t *testing.T) {
	e := setup(t, time.Minute, 5)
	id := e.submit(t, "hello", "", nil)
	e.waitWake(t)
	for _, tok := range []string{"", callerToken, webhookKey} {
		if code, _ := e.bot(t, "GET", "/bot/tasks", tok, ""); code != 401 {
			t.Errorf("token %q: status %d, want 401", tok, code)
		}
		if code, _ := e.bot(t, "POST", "/bot/tasks/"+string(id)+"/reply", tok, `{"state":"completed","text":"x"}`); code != 401 {
			t.Errorf("reply with token %q: status %d, want 401", tok, code)
		}
	}
	if code, got := e.bot(t, "GET", "/bot/tasks", botToken, ""); code != 200 || !strings.Contains(got, string(id)) {
		t.Fatalf("list: %d %s", code, got)
	}
	if code, _ := e.bot(t, "POST", "/bot/tasks/"+string(id)+"/reply", botToken, `{"state":"done"}`); code != 400 {
		t.Fatalf("bad state accepted: %d", code)
	}
}

func TestPushNotificationOnCompletion(t *testing.T) {
	e := setup(t, time.Minute, 5)
	got := make(chan string, 10)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r.Header.Get("A2A-Notification-Token") + " " + string(b)
	}))
	defer receiver.Close()

	id := e.submit(t, "hello", "", &a2a.PushConfig{URL: receiver.URL, Token: "push-secret"})
	e.waitWake(t)
	e.bot(t, "POST", "/bot/tasks/"+string(id)+"/reply", botToken, `{"state":"completed","text":"pushed"}`)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case n := <-got:
			if strings.HasPrefix(n, "push-secret ") && strings.Contains(n, "TASK_STATE_COMPLETED") {
				return
			}
		case <-deadline:
			t.Fatal("no push notification with completed state")
		}
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	if _, err := grokbot.New(grokbot.Config{Timeout: time.Minute, MaxPending: 1}); err == nil || !strings.Contains(err.Error(), "webhook URL") {
		t.Fatalf("err = %v", err)
	}
}
