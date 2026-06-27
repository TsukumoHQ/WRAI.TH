package ingest

import (
	"context"
)

type Config struct {
	EventBufferSize int
	SessionProvider SessionProvider
	// AgentResolver maps a live session_id → owning agent. Lets the detector tag
	// activity with the stable agent identity (not the rotating session_id), so
	// the UI joins activity to agents by name. Optional; nil → activity untagged.
	AgentResolver AgentResolver
}

func (c *Config) defaults() {
	if c.EventBufferSize <= 0 {
		c.EventBufferSize = 100
	}
}

// SessionProvider returns the set of known Claude session IDs from registered agents.
type SessionProvider func() map[string]bool

type Ingester struct {
	Events          chan AgentEvent
	detector        *Detector
	cancel          context.CancelFunc
	SessionProvider SessionProvider
}

func New(cfg Config) (*Ingester, error) {
	cfg.defaults()

	ctx, cancel := context.WithCancel(context.Background())

	// Events is the detector's lifecycle-event sink. Activity now arrives over
	// HTTP via RecordHookEvent (see handlers_ingest.go) — the old fsnotify
	// file-drop watcher is gone. Nothing consumes Events in production; the
	// detector's tick sends to it non-blockingly and drops when unread.
	events := make(chan AgentEvent, cfg.EventBufferSize)
	detector := newDetector(events, cfg.AgentResolver)

	go detector.run(ctx)

	return &Ingester{
		Events:          events,
		detector:        detector,
		cancel:          cancel,
		SessionProvider: cfg.SessionProvider,
	}, nil
}

// RecordHookEvent feeds an event that arrived over HTTP (a Claude Code hook
// POSTing to the relay) into the in-memory detector — same path the file-drop
// watcher used, minus the filesystem. Activity is ephemeral: it updates detector
// state + broadcasts to SSE subscribers, and never touches the DB.
func (i *Ingester) RecordHookEvent(evt AgentEvent) {
	i.detector.RecordEvent(evt)
}

func (i *Ingester) GetSessions() []SessionState {
	return i.detector.GetSessions()
}

func (i *Ingester) SubscribeActivity() chan []SessionState {
	return i.detector.Subscribe()
}

func (i *Ingester) UnsubscribeActivity(ch chan []SessionState) {
	i.detector.Unsubscribe(ch)
}

func (i *Ingester) Stop() {
	// Cancel the detector loop. We deliberately do NOT close(i.Events): an
	// in-flight tick could still send on it and panic on a closed channel. With
	// the loop cancelled there's no further producer and the channel is GC'd.
	i.cancel()
}
