package relay

import (
	"context"
	"errors"
	"sync"
	"time"

	"agent-relay/internal/db"
	"agent-relay/internal/models"
)

// Identity gate resilience (task 33dab33d). Field cause of the 'identity check
// failed: get agent: context deadline exceeded' refusals was host saturation
// (load 55 on 18 cores, swap 10.7/12 GB) stalling the relay process past
// readerTimeout — not the write lock: GetAgent reads the WAL reader pool
// (db/agents.go GetAgent), which a held write lock never blocks. A stall of a
// few seconds must not fail an authed call, so the gate (guardIdentity) now
// (1) retries the lookup once on a deadline error and (2) serves a repeat check
// of the same live agent from a short-TTL cache without touching the DB.

// identityCacheTTL bounds how long a cached identity row may grant access. An
// entry expires at insert time + TTL and is never extended by a hit, so a
// revoked/deactivated agent is refused at most identityCacheTTL after the
// revocation. var, not const: tests shrink it.
var identityCacheTTL = 3 * time.Second

// identityLookup is the gate's DB read. var so a test can make it time out once.
var identityLookup = func(d *db.DB, project, name string) (*models.Agent, error) {
	return d.GetAgent(project, name)
}

type identityEntry struct {
	agent   models.Agent
	expires time.Time
}

// identityCache maps project+"\x00"+name to the last eligible row read. Zero
// value is ready to use.
type identityCache struct {
	mu sync.Mutex
	m  map[string]identityEntry
}

func identityKey(project, name string) string { return project + "\x00" + name }

func (c *identityCache) get(project, name string, now time.Time) (*models.Agent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[identityKey(project, name)]
	if !ok {
		return nil, false
	}
	if !now.Before(e.expires) {
		delete(c.m, identityKey(project, name))
		return nil, false
	}
	a := e.agent // copy: callers never share the cached row
	return &a, true
}

func (c *identityCache) put(project, name string, a *models.Agent, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]identityEntry{}
	}
	// Opportunistic sweep keeps the map bounded by the agents seen in one TTL.
	if len(c.m) > 512 {
		for k, e := range c.m {
			if !now.Before(e.expires) {
				delete(c.m, k)
			}
		}
	}
	c.m[identityKey(project, name)] = identityEntry{agent: *a, expires: now.Add(identityCacheTTL)}
}

// invalidate drops a cached row so the next check reads the DB. Called from
// cascadeDeactivatedAgent (every deactivate/delete path). Other status changes
// (e.g. sleep) are bounded by identityCacheTTL; register needs no hook because
// refusals are never cached.
func (c *identityCache) invalidate(project, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, identityKey(project, name))
}

// identityAgent is the gate's identity read: cache hit, else GetAgent retried
// once on a deadline error. Only rows SenderEligibility accepts are cached — a
// refusal (unregistered, inactive, deleted) is always decided from a fresh read,
// so a just-registered or just-reactivated agent is never refused from a stale
// entry; the cache can only grant, and only for identityCacheTTL.
func (h *Handlers) identityAgent(project, name string) (*models.Agent, error) {
	if a, ok := h.idCache.get(project, name, time.Now()); ok {
		return a, nil
	}
	a, err := identityLookup(h.db, project, name)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		a, err = identityLookup(h.db, project, name)
	}
	if err != nil {
		return nil, err
	}
	if eligible, _ := db.SenderEligibility(a); eligible {
		h.idCache.put(project, name, a, time.Now())
	}
	return a, nil
}
