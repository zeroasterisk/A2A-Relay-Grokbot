// Package grokbot is a relay backend for a Grok Bot reached through a routine
// webhook trigger.
//
// The webhook body only tells the bot that a task is waiting and where. The
// task text itself is fetched from the relay with the bot's own token, so
// someone who learns the webhook key can wake the bot but cannot hand it work.
package grokbot

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zeroasterisk/A2A-Relay-Grokbot/relay"
)

// Config holds everything the backend needs. All fields are required.
type Config struct {
	WebhookURL string        // routine trigger URL from the Grok Bot app
	WebhookKey string        // routine key (crsr_...) sent as a bearer token
	BotToken   string        // secret the bot uses to read and answer tasks
	PublicURL  string        // relay base URL the bot can reach
	Timeout    time.Duration // how long to wait for the bot before failing the task
	MaxPending int           // cap on tasks waiting for the bot (each one is a paid wake)
	Client     *http.Client  // optional; defaults to a 15s-timeout client
}

// ErrInboxFull is returned when MaxPending tasks are already waiting.
var ErrInboxFull = errors.New("grok bot inbox is full, try again later")

type job struct {
	relay.Request
	Created time.Time `json:"created_at"`
	claimed func()
	reply   chan relay.Reply
	once    sync.Once
}

// Backend implements relay.Backend and serves the bot-facing inbox API.
type Backend struct {
	cfg  Config
	mu   sync.Mutex
	jobs map[string]*job
}

// New validates cfg and returns a backend.
func New(cfg Config) (*Backend, error) {
	var missing []string
	for name, v := range map[string]string{
		"webhook URL": cfg.WebhookURL, "webhook key": cfg.WebhookKey,
		"bot token": cfg.BotToken, "public URL": cfg.PublicURL,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("grokbot: missing %s", strings.Join(missing, ", "))
	}
	if cfg.Timeout <= 0 || cfg.MaxPending <= 0 {
		return nil, errors.New("grokbot: timeout and max pending must be positive")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 15 * time.Second}
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	return &Backend{cfg: cfg, jobs: map[string]*job{}}, nil
}

// Handle implements relay.Backend: park the request, wake the bot, wait.
func (b *Backend) Handle(ctx context.Context, req relay.Request, claimed func()) (relay.Reply, error) {
	j := &job{Request: req, Created: time.Now().UTC(), claimed: claimed, reply: make(chan relay.Reply, 1)}
	b.mu.Lock()
	if len(b.jobs) >= b.cfg.MaxPending {
		b.mu.Unlock()
		return relay.Reply{}, ErrInboxFull
	}
	b.jobs[req.TaskID] = j
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.jobs, req.TaskID)
		b.mu.Unlock()
	}()

	if err := b.wake(ctx, req.TaskID); err != nil {
		// The detail can contain the routine URL; keep it out of the task
		// status, which the caller sees.
		log.Printf("grokbot: wake for task %s failed: %v", req.TaskID, err)
		return relay.Reply{}, errors.New("could not wake the grok bot (see relay logs)")
	}

	timer := time.NewTimer(b.cfg.Timeout)
	defer timer.Stop()
	select {
	case r := <-j.reply:
		return r, nil
	case <-timer.C:
		return relay.Reply{}, fmt.Errorf("grok bot did not answer within %s", b.cfg.Timeout)
	case <-ctx.Done():
		return relay.Reply{}, ctx.Err()
	}
}

func (b *Backend) wake(ctx context.Context, taskID string) error {
	body, err := json.Marshal(map[string]string{
		"event":    "agentmsg.task",
		"task_id":  taskID,
		"task_url": b.cfg.PublicURL + "/bot/tasks/" + taskID,
	})
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+b.cfg.WebhookKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.cfg.Client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}

// Routes registers the bot-facing API on mux:
//
//	GET  /bot/tasks            list waiting tasks
//	GET  /bot/tasks/{id}       read one task (marks it claimed -> A2A "working")
//	POST /bot/tasks/{id}/reply {"state": "...", "text": "..."}
func (b *Backend) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /bot/tasks", b.authed(b.list))
	mux.HandleFunc("GET /bot/tasks/{id}", b.authed(b.get))
	mux.HandleFunc("POST /bot/tasks/{id}/reply", b.authed(b.replyTo))
}

func (b *Backend) authed(h http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + b.cfg.BotToken)
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (b *Backend) list(w http.ResponseWriter, _ *http.Request) {
	b.mu.Lock()
	out := make([]*job, 0, len(b.jobs))
	for _, j := range b.jobs {
		out = append(out, j)
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, k int) bool { return out[i].Created.Before(out[k].Created) })
	writeJSON(w, out)
}

func (b *Backend) find(w http.ResponseWriter, r *http.Request) *job {
	b.mu.Lock()
	j := b.jobs[r.PathValue("id")]
	b.mu.Unlock()
	if j == nil {
		http.Error(w, "no such task waiting (answered, canceled, timed out, or relay restarted)", http.StatusNotFound)
	}
	return j
}

func (b *Backend) get(w http.ResponseWriter, r *http.Request) {
	if j := b.find(w, r); j != nil {
		j.claimed()
		writeJSON(w, j)
	}
}

func (b *Backend) replyTo(w http.ResponseWriter, r *http.Request) {
	j := b.find(w, r)
	if j == nil {
		return
	}
	var rep relay.Reply
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&rep); err != nil {
		http.Error(w, "body must be JSON {\"state\":..., \"text\":...}", http.StatusBadRequest)
		return
	}
	switch rep.State {
	case relay.ReplyCompleted, relay.ReplyInputRequired, relay.ReplyRejected, relay.ReplyFailed:
	default:
		http.Error(w, "state must be completed, input-required, rejected, or failed", http.StatusBadRequest)
		return
	}
	accepted := false
	j.once.Do(func() {
		j.claimed()
		j.reply <- rep
		accepted = true
	})
	if !accepted {
		http.Error(w, "task already answered", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
