package ingest

import (
	"context"
	"sync"
	"time"
)

const (
	waitingThreshold = 10 * time.Second
	idleThreshold    = 30 * time.Second
	exitThreshold    = 5 * time.Minute
	tickInterval     = 2 * time.Second
	minDisplayTime   = 1500 * time.Millisecond // activity visible for at least 1.5s
)

type SessionState struct {
	SessionID string    `json:"session_id"`
	Activity  Activity  `json:"activity"`
	Tool      string    `json:"tool"`
	File      string    `json:"file"`
	LastEvent time.Time `json:"last_event"`
	State     string    `json:"state"` // "active", "idle", "waiting", "exited"
}

type sessionEntry struct {
	lastEvent    time.Time
	lastType     EventType
	tool         string
	file         string
	activity     Activity
	state        string
	idleSent     bool
	waitSent     bool
	displayUntil time.Time // activity stays visible until this time
	pendingType  EventType // deferred event waiting for display to expire
	pendingAct   Activity
}

type Detector struct {
	mu          sync.RWMutex
	sessions    map[string]*sessionEntry
	out         chan<- AgentEvent
	subMu       sync.RWMutex
	subscribers map[chan []SessionState]struct{}
}

func newDetector(out chan<- AgentEvent) *Detector {
	return &Detector{
		sessions:    make(map[string]*sessionEntry),
		out:         out,
		subscribers: make(map[chan []SessionState]struct{}),
	}
}

// Subscribe returns a channel that receives session state snapshots on every change.
func (d *Detector) Subscribe() chan []SessionState {
	ch := make(chan []SessionState, 8)
	d.subMu.Lock()
	d.subscribers[ch] = struct{}{}
	d.subMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (d *Detector) Unsubscribe(ch chan []SessionState) {
	d.subMu.Lock()
	delete(d.subscribers, ch)
	d.subMu.Unlock()
	close(ch)
}

// broadcast sends current state to all SSE subscribers (non-blocking).
func (d *Detector) broadcast() {
	d.subMu.RLock()
	defer d.subMu.RUnlock()
	if len(d.subscribers) == 0 {
		return
	}
	snap := d.getSessionsLocked()
	for ch := range d.subscribers {
		select {
		case ch <- snap:
		default:
			// subscriber too slow, skip
		}
	}
}

func (d *Detector) getSessionsLocked() []SessionState {
	result := make([]SessionState, 0, len(d.sessions))
	for sid, s := range d.sessions {
		result = append(result, SessionState{
			SessionID: sid,
			Activity:  s.activity,
			Tool:      s.tool,
			File:      s.file,
			LastEvent: s.lastEvent,
			State:     s.state,
		})
	}
	return result
}

func (d *Detector) RecordEvent(evt AgentEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()

	s, ok := d.sessions[evt.SessionID]
	if !ok {
		s = &sessionEntry{}
		d.sessions[evt.SessionID] = s
	}

	s.lastEvent = evt.Timestamp
	s.lastType = evt.Type
	s.file = evt.File
	s.idleSent = false
	s.waitSent = false

	now := time.Now()

	switch evt.Type {
	case EventStop:
		// Agent turn ended → waiting for user input (always immediate)
		s.tool = ""
		s.activity = ActivityWaiting
		s.state = "waiting"
		s.displayUntil = time.Time{}
		s.pendingType = ""
	case EventToolEnd:
		// Tool finished — but keep current activity visible for minDisplayTime
		if now.Before(s.displayUntil) {
			s.pendingType = EventToolEnd
			s.pendingAct = ActivityThinking
		} else {
			s.tool = ""
			s.activity = ActivityThinking
			s.state = "thinking"
		}
	default:
		// tool_start — new activity always wins, set minimum display
		s.tool = evt.Tool
		s.activity = evt.Activity
		s.state = "active"
		s.displayUntil = now.Add(minDisplayTime)
		s.pendingType = ""
	}

	d.broadcast()
}

func (d *Detector) GetSessions() []SessionState {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make([]SessionState, 0, len(d.sessions))
	for sid, s := range d.sessions {
		result = append(result, SessionState{
			SessionID: sid,
			Activity:  s.activity,
			Tool:      s.tool,
			File:      s.file,
			LastEvent: s.lastEvent,
			State:     s.state,
		})
	}
	return result
}

func (d *Detector) run(ctx context.Context) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d.tick(now)
		}
	}
}

func (d *Detector) tick(now time.Time) {
	// Collect channel events while holding the lock, but emit them only after
	// releasing it: sending on d.out while holding d.mu means a stalled consumer
	// (buffer full) would block tick with the lock held, head-of-line-blocking
	// every RecordEvent/GetSessions. broadcast() stays under the lock because it
	// reads d.sessions via getSessionsLocked.
	var pending []AgentEvent
	d.mu.Lock()
	for sid, s := range d.sessions {
		elapsed := now.Sub(s.lastEvent)

		// Flush pending transitions when display time expires
		if s.pendingType != "" && now.After(s.displayUntil) {
			s.tool = ""
			s.activity = s.pendingAct
			s.state = "thinking"
			s.pendingType = ""
		}

		if elapsed > exitThreshold {
			if s.state != "exited" {
				s.state = "exited"
				s.activity = ActivityIdle
				pending = append(pending, AgentEvent{
					Type:      EventAgentExit,
					SessionID: sid,
					Activity:  ActivityIdle,
					Timestamp: now,
				})
			}
			delete(d.sessions, sid)
			continue
		}

		if elapsed > idleThreshold && !s.idleSent {
			s.idleSent = true
			s.state = "idle"
			s.activity = ActivityIdle
			pending = append(pending, AgentEvent{
				Type:      EventIdle,
				SessionID: sid,
				Activity:  ActivityIdle,
				Timestamp: now,
			})
			continue
		}

		if elapsed > waitingThreshold && s.lastType == EventToolEnd && !s.waitSent {
			s.waitSent = true
			s.state = "waiting"
			s.activity = ActivityWaiting
			pending = append(pending, AgentEvent{
				Type:      EventWaiting,
				SessionID: sid,
				Activity:  ActivityWaiting,
				Timestamp: now,
			})
		}
	}

	d.broadcast()
	d.mu.Unlock()

	for _, ev := range pending {
		d.out <- ev
	}
}
