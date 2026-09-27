package relay

import (
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// A2APath is where the JSON-RPC endpoint is mounted.
const A2APath = "/a2a"

// Options configures NewServer.
type Options struct {
	PublicURL   string // base URL callers use to reach the relay
	Name        string
	Description string
	Backend     Backend
	Auth        *CallerAuth
	// PushAllowPrivate lets push notifications reach loopback/private
	// addresses. Leave false in production (SSRF guard).
	PushAllowPrivate bool
}

// NewServer wires the SDK handler, agent card, caller auth, per-caller task
// store, and push notifications into one http.ServeMux.
func NewServer(o Options) *http.ServeMux {
	base := strings.TrimRight(o.PublicURL, "/")
	card := &a2a.AgentCard{
		Name:        o.Name,
		Description: o.Description,
		Version:     "0.1.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(base+A2APath, a2a.TransportProtocolJSONRPC),
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Capabilities:       a2a.AgentCapabilities{Streaming: true, PushNotifications: true},
		SecuritySchemes: a2a.NamedSecuritySchemes{
			"bearer": a2a.HTTPAuthSecurityScheme{Scheme: "Bearer", Description: "Per-caller token issued by the relay operator."},
		},
		SecurityRequirements: a2a.SecurityRequirementsOptions{{"bearer": {}}},
		Skills: []a2a.AgentSkill{{
			ID:          "delegate",
			Name:        "Delegate a task",
			Description: "Hands a plain-text task to the agent behind this relay. Replies can take minutes; use streaming, push notifications, or poll the task.",
			Tags:        []string{"relay"},
		}},
	}

	handler := a2asrv.NewHandler(
		&Executor{Backend: o.Backend},
		a2asrv.WithCallInterceptors(o.Auth),
		a2asrv.WithTaskStore(taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
			Authenticator: a2asrv.NewTaskStoreAuthenticator(),
		})),
		a2asrv.WithPushNotifications(push.NewInMemoryStore(),
			push.NewHTTPPushSender(&push.HTTPSenderConfig{AllowPrivateNetworks: o.PushAllowPrivate})),
	)

	mux := http.NewServeMux()
	mux.Handle(A2APath, a2asrv.NewJSONRPCHandler(handler))
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}
