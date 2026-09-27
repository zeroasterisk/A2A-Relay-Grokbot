package relay_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/zeroasterisk/A2A-Relay-Grokbot/relay"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func client(t *testing.T, url, token string) *a2aclient.Client {
	t.Helper()
	c, err := a2aclient.NewFromEndpoints(context.Background(),
		[]*a2a.AgentInterface{a2a.NewAgentInterface(url+relay.A2APath, a2a.TransportProtocolJSONRPC)},
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: bearer{token}}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	auth, err := relay.NewCallerAuth("alice=a-token, bob=b-token")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(nil)
	srv.Config.Handler = relay.NewServer(relay.Options{PublicURL: srv.URL, Name: "t", Backend: relay.Echo{}, Auth: auth})
	t.Cleanup(srv.Close)
	return srv
}

func send(ctx context.Context, c *a2aclient.Client, text string) (a2a.SendMessageResult, error) {
	return c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))})
}

func TestEchoCompletesWithArtifact(t *testing.T) {
	srv := echoServer(t)
	res, err := send(context.Background(), client(t, srv.URL, "a-token"), "ping")
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("want *a2a.Task, got %T", res)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", task.Status.State)
	}
	if len(task.Artifacts) != 1 || task.Artifacts[0].Parts[0].Text() != "ping" {
		t.Fatalf("artifacts = %+v", task.Artifacts)
	}
}

func TestUnknownOrMissingTokenIsRejected(t *testing.T) {
	srv := echoServer(t)
	for _, tok := range []string{"", "wrong"} {
		if _, err := send(context.Background(), client(t, srv.URL, tok), "ping"); err == nil {
			t.Errorf("token %q: want error, got success", tok)
		}
	}
}

func TestCallersCannotSeeEachOthersTasks(t *testing.T) {
	srv := echoServer(t)
	ctx := context.Background()
	res, err := send(ctx, client(t, srv.URL, "a-token"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	id := res.(*a2a.Task).ID
	if _, err := client(t, srv.URL, "b-token").GetTask(ctx, &a2a.GetTaskRequest{ID: id}); err == nil {
		t.Fatal("bob read alice's task")
	}
	if _, err := client(t, srv.URL, "a-token").GetTask(ctx, &a2a.GetTaskRequest{ID: id}); err != nil {
		t.Fatalf("alice cannot read her own task: %v", err)
	}
}

func TestAgentCardIsPublicAndDeclaresBearer(t *testing.T) {
	srv := echoServer(t)
	resp, err := http.Get(srv.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if _, ok := card.SecuritySchemes["bearer"]; !ok || card.SupportedInterfaces[0].URL != srv.URL+relay.A2APath {
		t.Fatalf("card = %+v", card)
	}
}

func TestNewCallerAuth(t *testing.T) {
	for _, spec := range []string{"", " , ", "noequals", "=tok", "name="} {
		if _, err := relay.NewCallerAuth(spec); err == nil {
			t.Errorf("spec %q: want error", spec)
		}
	}
}
