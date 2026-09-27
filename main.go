// Command agentmsg runs an A2A relay in front of an agent that has no native
// A2A endpoint. Configuration is read from environment variables; see README.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/zeroasterisk/A2A-Relay-Grokbot/grokbot"
	"github.com/zeroasterisk/A2A-Relay-Grokbot/relay"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	port := env("PORT", "8080")
	publicURL := env("AGENTMSG_PUBLIC_URL", "http://localhost:"+port)

	auth, err := relay.NewCallerAuth(os.Getenv("AGENTMSG_CALLERS"))
	if err != nil {
		return fmt.Errorf("AGENTMSG_CALLERS: %w", err)
	}
	pushPrivate, err := strconv.ParseBool(env("AGENTMSG_PUSH_ALLOW_PRIVATE", "false"))
	if err != nil {
		return fmt.Errorf("AGENTMSG_PUSH_ALLOW_PRIVATE: %w", err)
	}

	opts := relay.Options{PublicURL: publicURL, Auth: auth, PushAllowPrivate: pushPrivate}
	var gb *grokbot.Backend
	switch backend := env("AGENTMSG_BACKEND", "echo"); backend {
	case "echo":
		opts.Backend = relay.Echo{}
		opts.Name, opts.Description = "agentmsg echo", "A2A relay with the echo backend: replies with your text."
	case "grokbot":
		if os.Getenv("AGENTMSG_PUBLIC_URL") == "" {
			return errors.New("AGENTMSG_PUBLIC_URL is required with the grokbot backend: the bot fetches tasks from it")
		}
		timeout, err := time.ParseDuration(env("GROKBOT_TIMEOUT", "30m"))
		if err != nil {
			return fmt.Errorf("GROKBOT_TIMEOUT: %w", err)
		}
		maxPending, err := strconv.Atoi(env("GROKBOT_MAX_PENDING", "10"))
		if err != nil {
			return fmt.Errorf("GROKBOT_MAX_PENDING: %w", err)
		}
		gb, err = grokbot.New(grokbot.Config{
			WebhookURL: os.Getenv("GROKBOT_WEBHOOK_URL"),
			WebhookKey: os.Getenv("GROKBOT_WEBHOOK_KEY"),
			BotToken:   os.Getenv("GROKBOT_BOT_TOKEN"),
			PublicURL:  publicURL,
			Timeout:    timeout,
			MaxPending: maxPending,
		})
		if err != nil {
			return err
		}
		opts.Backend = gb
		opts.Name = env("AGENTMSG_NAME", "Grok Bot (via agentmsg)")
		opts.Description = env("AGENTMSG_DESCRIPTION", "A Grok Bot teammate reachable over A2A. Each task wakes the bot; expect replies in minutes, not seconds.")
	default:
		return errors.New("AGENTMSG_BACKEND must be echo or grokbot")
	}

	mux := relay.NewServer(opts)
	if gb != nil {
		gb.Routes(mux)
	}
	log.Printf("agentmsg listening on :%s, public URL %s", port, publicURL)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
