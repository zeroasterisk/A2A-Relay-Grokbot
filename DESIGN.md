# A2A Relay for Grok Bot: intent and design

## Intent

Let any A2A client hand work to an agent that has no A2A endpoint of its own,
starting with a Grok Bot. The relay is a normal A2A server built on the
official Go SDK. Everything agent-specific lives behind a small `Backend`
interface; Grok Bot is the first backend.

Done means (each is checked by a test or a command in the README):

- VAL-1 Given a caller with a valid token, when it sends a message, then it
  gets an A2A task back without waiting for the bot.
- VAL-2 Given no token or an unknown token, the relay refuses the call. The
  agent card stays public.
- VAL-3 A caller can only read its own tasks.
- VAL-4 A task says `submitted` until the bot has actually read it, `working`
  after, and `completed` only when the bot posted a result.
- VAL-5 The webhook that wakes the bot carries no task text. The bot reads the
  task from the relay with its own token, so a leaked webhook key can wake the
  bot but cannot give it work.
- VAL-6 The bot can answer `completed`, `input-required` (caller must answer,
  same task continues), `rejected` (e.g. its human said no), or `failed`.
- VAL-7 Failures are reported, never hidden: webhook error, bot timeout, full
  inbox, and cancel each end the task in a clear state. Webhook errors don't
  leak the routine URL to callers.
- VAL-8 A2A push notifications fire on state changes.
- VAL-9 The echo backend makes the relay usable end to end with no bot.

## What was reused

- `github.com/a2aproject/a2a-go/v2` v2.5.0: JSON-RPC transport, task store
  (with per-caller ownership via `NewTaskStoreAuthenticator`), streaming,
  cancel, push notifications with the SDK's SSRF guard, agent card handler.
- The SDK's `a2a` CLI is the client in every manual check, and is what a Grok
  Bot installs to call other A2A agents (the outbound direction needs no relay
  code).
- Checked and not reused: `a2a serve --exec` (runs a subprocess per task, no
  caller auth, no way for the bot to pull work), `a2a serve --proxy` (needs an
  A2A upstream), a2a-samples (no gateway or relay sample; the Go
  sign-and-verify-agent-card sample is the reference for card signing later).

## Shape

```
A2A caller --JSON-RPC /a2a--> relay (a2asrv handler + CallerAuth)
                                 |  Executor -> Backend.Handle(req, claimed)
                                 v
                        grokbot backend: inbox (memory)
                                 |  POST webhook {task_id, task_url}  (Bearer crsr_...)
                                 v
                           Grok Bot routine wakes
                                 |  GET  /bot/tasks/{id}        (Bearer bot token) -> "working"
                                 |  POST /bot/tasks/{id}/reply  {"state","text"}
                                 v
                  task completed / input-required / rejected / failed (+ push)
```

`Backend.Handle` blocks until the reply, timeout, or cancel. The SDK runs each
execution in its own goroutine, and A2A's async lifecycle (return immediately,
poll, stream, or push) absorbs the bot's wake latency.

## Deliberate limits (v0.1)

- **In-memory state.** A restart loses open tasks; the bot's reply then gets
  a 404 that says so. Run one instance that stays up (Cloud Run:
  `--min-instances=1 --max-instances=1 --no-cpu-throttling`, or a small VM).
  A durable store is the SDK's `taskstore.Store` interface plus a table for
  the inbox.
- **No signed agent card.** The SDK has the `signatures` field but no signer;
  the official sample's signer is the path when it's worth adding key
  management.
- **Static tokens.** Callers and the bot use bearer tokens from env vars.
  Good enough to control who can wake a paid bot; not an identity system.
- **Text only.** The bot sees text parts and replies with text.
- **JSON-RPC only.** One transport keeps the card and routes simple; the CLI
  and SDK clients pick it up from the card.
- **Webhook shape is from third-party docs.** The routine URL, `crsr_` bearer
  key, and "request body becomes the run's input" come from Hookdeck's guide,
  not xAI docs, and have not been tried against a live bot.
