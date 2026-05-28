package spawn

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agent-relay/internal/db"
	"agent-relay/internal/lock"
	"agent-relay/internal/scheduler"

	"github.com/google/uuid"
)

// ChildState tracks a running spawned child process.
type ChildState struct {
	ID        string
	Parent    string
	Project   string
	Profile   string
	Prompt    string
	StartedAt time.Time
	Cancel    context.CancelFunc
}

// Manager manages agent spawning, scheduling, and pool limits.
type Manager struct {
	mu        sync.RWMutex
	children  map[string]*ChildState // childID -> state
	executor  *Executor
	db        *db.DB
	lockMgr   *lock.Manager
	queue     *lock.PriorityQueue
	scheduler *scheduler.Scheduler
	live      *LiveBuffer
	metrics   *MetricsCollector
	logger    *slog.Logger
	maxPool   int // global max concurrent children
}

// NewManager creates a spawn manager.
func NewManager(database *db.DB, executor *Executor, lockMgr *lock.Manager, queue *lock.PriorityQueue, sched *scheduler.Scheduler, maxPool int, logger *slog.Logger) *Manager {
	return &Manager{
		children:  make(map[string]*ChildState),
		executor:  executor,
		db:        database,
		lockMgr:   lockMgr,
		queue:     queue,
		scheduler: sched,
		live:      NewLiveBuffer(),
		metrics:   NewMetricsCollector(1000),
		logger:    logger,
		maxPool:   maxPool,
	}
}

// Spawn creates a child agent process. Returns the child ID.
func (m *Manager) Spawn(parentAgent, project, profile, prompt, ttlStr, allowedTools string) (string, error) {
	m.mu.Lock()

	// Check global pool limit
	activeCount := 0
	for _, c := range m.children {
		if c.Project == project {
			activeCount++
		}
	}
	if activeCount >= m.maxPool {
		m.mu.Unlock()
		return "", fmt.Errorf("pool full: %d/%d active children in project %s", activeCount, m.maxPool, project)
	}

	childID := uuid.New().String()
	ctx, cancel := context.WithCancel(context.Background())

	child := &ChildState{
		ID:        childID,
		Parent:    parentAgent,
		Project:   project,
		Profile:   profile,
		Prompt:    prompt,
		StartedAt: time.Now().UTC(),
		Cancel:    cancel,
	}
	m.children[childID] = child
	m.mu.Unlock()

	// Record in DB
	m.db.InsertSpawnChild(childID, parentAgent, project, profile, prompt)

	ttl := scheduler.ParseTTL(ttlStr)

	// Build the full prompt with agent context
	fullPrompt := fmt.Sprintf(`You are a spawned child agent.
Profile: %s
Parent agent: %s
Project: %s

Pass as: "%s-child-%s" and project: "%s" on EVERY relay tool call.

Boot sequence:
1. Register with the relay: register_agent(name: "%s-child-%s", role: "%s", reports_to: "%s", project: "%s")
2. Execute your task below.
3. When done, report results to your parent via send_message.

## Task
%s
`, profile, parentAgent, project, profile, childID[:8], project, profile, childID[:8], profile, parentAgent, project, prompt)

	// Spawn in background goroutine
	go func() {
		m.live.Start(childID, "spawn")

		params := SpawnParams{
			Prompt:       fullPrompt,
			TTL:          ttl,
			AllowedTools: allowedTools,
			Streaming:    true,
		}

		result := m.executor.RunWithLive(ctx, params, m.live.Writer(childID))
		m.live.Finish(childID)

		// Record metrics
		metric := CycleMetric{
			Agent:               parentAgent,
			Project:             project,
			Cycle:               "spawn:" + profile,
			Duration:            result.Duration,
			Success:             result.ExitCode == 0,
			ExitCode:            result.ExitCode,
			Timestamp:           time.Now(),
			InputTokens:         result.Tokens.InputTokens,
			OutputTokens:        result.Tokens.OutputTokens,
			CacheReadTokens:     result.Tokens.CacheReadTokens,
			CacheCreationTokens: result.Tokens.CacheCreationTokens,
		}
		if result.Err != nil {
			metric.Error = result.Err.Error()
		}
		m.metrics.Record(metric)

		// Update DB
		errMsg := ""
		if result.Err != nil {
			errMsg = result.Err.Error()
		}
		m.db.UpdateSpawnChild(childID, "finished", result.ExitCode, errMsg)

		// Record cycle history
		m.db.RecordCycleHistory(parentAgent, project, "spawn:"+profile,
			result.Duration.Milliseconds(), result.ExitCode == 0, result.ExitCode, errMsg,
			result.Tokens.InputTokens, result.Tokens.OutputTokens,
			result.Tokens.CacheReadTokens, result.Tokens.CacheCreationTokens)

		// Cleanup in-memory state
		m.mu.Lock()
		delete(m.children, childID)
		m.mu.Unlock()

		m.logger.Info("child finished",
			"child_id", childID,
			"profile", profile,
			"exit_code", result.ExitCode,
			"duration", result.Duration,
		)
	}()

	m.logger.Info("child spawned",
		"child_id", childID,
		"parent", parentAgent,
		"profile", profile,
		"project", project,
	)

	return childID, nil
}

// KillChild terminates a running child by ID.
func (m *Manager) KillChild(childID string) error {
	m.mu.RLock()
	child, ok := m.children[childID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("child %s not found or already finished", childID)
	}

	child.Cancel()
	m.db.UpdateSpawnChild(childID, "killed", -1, "killed by parent")

	m.mu.Lock()
	delete(m.children, childID)
	m.mu.Unlock()

	m.logger.Info("child killed", "child_id", childID, "profile", child.Profile)
	return nil
}

// ListChildren returns children for a given parent agent and project.
func (m *Manager) ListChildren(parentAgent, project, status string) []map[string]any {
	return m.db.ListSpawnChildren(parentAgent, project, status)
}

// ActiveCount returns the number of currently running children in a project.
func (m *Manager) ActiveCount(project string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, c := range m.children {
		if c.Project == project {
			count++
		}
	}
	return count
}

// GetLiveBuffer returns the live output buffer.
func (m *Manager) GetLiveBuffer() *LiveBuffer {
	return m.live
}

// GetMetrics returns the metrics collector.
func (m *Manager) GetMetrics() *MetricsCollector {
	return m.metrics
}

// GetScheduler returns the scheduler.
func (m *Manager) GetScheduler() *scheduler.Scheduler {
	return m.scheduler
}

// GetExecutor returns the executor.
func (m *Manager) GetExecutor() *Executor {
	return m.executor
}

// WakeAndExecute wakes a seed agent and runs a cycle under its canonical
// identity — the claude process operates as `agentName` directly, with no
// row inserted in the `agents` table for an ephemeral child. After the cycle
// completes (success, failure, or TTL expiration), the agent is put back to
// sleep. This is the asymmetric counterpart to Spawn() / SpawnWithContext():
// same execution pipeline, different identity semantics (seed-as-self, not
// fork-as-child).
//
// Caller chooses between Agent OS mode (cycleName set, prompt empty) and
// legacy mode (prompt set, cycleName empty). At least one must be provided
// — for a pure DB-state wake without launching claude, call db.WakeAgent
// directly (the HandleWakeAgent MCP handler exposes both modes).
//
// Non-blocking : the cycle runs in a goroutine, this method returns as soon
// as the wake transition is recorded. Errors during execution are logged ;
// they propagate to cycle_history but not to the immediate caller.
func (m *Manager) WakeAndExecute(agentName, project, prompt, ttlStr, cycleName, allowedTools string) error {
	if prompt == "" && cycleName == "" {
		return fmt.Errorf("WakeAndExecute requires either prompt or cycleName")
	}

	// Transition the seed to active. Idempotent : 0 rows affected is fine
	// (already active, or about to be re-activated by the cycle's own
	// register_agent if it happens). We don't fail on already-active.
	if _, err := m.db.WakeAgent(project, agentName); err != nil {
		return fmt.Errorf("wake DB transition: %w", err)
	}

	// Run the cycle in background under the seed identity. The body reuses
	// the same machinery as scheduled cycles (locks, queue, metrics, history,
	// Agent OS / legacy prompt branching). When it returns, put the seed back
	// to sleep — the canonical wake/work/sleep cadence.
	go func() {
		m.executeCycle(agentName, project, "wake", prompt, ttlStr, cycleName, allowedTools)
		if err := m.db.SleepAgent(project, agentName); err != nil {
			m.logger.Warn("sleep after wake_agent cycle failed", "agent", agentName, "error", err)
		}
	}()

	m.logger.Info("wake_agent triggered",
		"agent", agentName,
		"project", project,
		"mode", func() string {
			if cycleName != "" {
				return "agent-os"
			}
			return "legacy"
		}(),
	)
	return nil
}

// SpawnWithContext assembles context from DB and spawns an agent.
// This is the Agent OS "exec" — takes profile + cycle, builds the full prompt.
func (m *Manager) SpawnWithContext(project, profileSlug, cycleName, taskID string) (string, error) {
	// Build the full context object
	ctx, err := BuildSpawnContext(m.db, project, profileSlug, cycleName, taskID, ModeHeadless)
	if err != nil {
		return "", fmt.Errorf("build context: %w", err)
	}

	// Format as prompt
	prompt := FormatPrompt(ctx)

	// Check per-profile pool limit
	profile, _ := m.db.GetProfile(project, profileSlug)
	if profile != nil && profile.PoolSize > 0 {
		m.mu.RLock()
		profileCount := 0
		for _, c := range m.children {
			if c.Project == project && c.Profile == profileSlug {
				profileCount++
			}
		}
		m.mu.RUnlock()
		if profileCount >= profile.PoolSize {
			return "", fmt.Errorf("profile pool full: %d/%d active for %s", profileCount, profile.PoolSize, profileSlug)
		}
	}

	// TTL from cycle DB or default
	ttl := "10m"
	if cycleName != "" {
		if cycle, _ := m.db.GetCycle(project, cycleName); cycle != nil && cycle.TTL > 0 {
			ttl = fmt.Sprintf("%dm", cycle.TTL)
		}
	}

	return m.Spawn("relay-os", project, profileSlug, prompt, ttl, "")
}

// --- Schedule management ---

// Schedule creates or updates a cron schedule and registers it with the scheduler.
func (m *Manager) Schedule(id, agentName, project, name, cronExpr, prompt, ttl, cycle, allowedTools string) error {
	// Store in DB
	m.db.UpsertSchedule(id, agentName, project, name, cronExpr, prompt, ttl, cycle, allowedTools)

	// Register with cron
	return m.scheduler.AddJob(id, cronExpr, func() {
		m.executeCycle(agentName, project, name, prompt, ttl, cycle, allowedTools)
	})
}

// Unschedule removes a schedule.
func (m *Manager) Unschedule(scheduleID string) {
	m.scheduler.RemoveJob(scheduleID)
	m.db.DeleteSchedule(scheduleID)
}

// TriggerCycle manually triggers a scheduled cycle.
func (m *Manager) TriggerCycle(scheduleID string) error {
	sched := m.db.GetSchedule(scheduleID)
	if sched == nil {
		return fmt.Errorf("schedule %s not found", scheduleID)
	}

	agentName, _ := sched["agent_name"].(string)
	project, _ := sched["project"].(string)
	name, _ := sched["name"].(string)
	prompt, _ := sched["prompt"].(string)
	ttl, _ := sched["ttl"].(string)
	cycle, _ := sched["cycle"].(string)
	allowedTools, _ := sched["allowed_tools"].(string)

	go m.executeCycle(agentName, project, name, prompt, ttl, cycle, allowedTools)
	return nil
}

// LoadSchedulesFromDB loads all enabled schedules from DB and registers them.
func (m *Manager) LoadSchedulesFromDB() {
	schedules := m.db.ListAllEnabledSchedules()
	for _, s := range schedules {
		id, _ := s["id"].(string)
		agentName, _ := s["agent_name"].(string)
		project, _ := s["project"].(string)
		name, _ := s["name"].(string)
		cronExpr, _ := s["cron_expr"].(string)
		prompt, _ := s["prompt"].(string)
		ttl, _ := s["ttl"].(string)
		cycle, _ := s["cycle"].(string)
		allowedTools, _ := s["allowed_tools"].(string)

		err := m.scheduler.AddJob(id, cronExpr, func() {
			m.executeCycle(agentName, project, name, prompt, ttl, cycle, allowedTools)
		})
		if err != nil {
			m.logger.Error("failed to load schedule", "id", id, "error", err)
		}
	}
	m.logger.Info("schedules loaded from DB", "count", len(schedules))
}

// executeCycle handles lock acquisition, execution, metrics, and queue processing.
func (m *Manager) executeCycle(agentName, project, cycleName, prompt, ttlStr, cycle, allowedTools string) {
	ttl := scheduler.ParseTTL(ttlStr)

	lk, err := m.lockMgr.Acquire(agentName, cycleName, ttl)
	if err != nil {
		m.logger.Error("lock error", "agent", agentName, "cycle", cycleName, "error", err)
		return
	}
	if lk == nil {
		m.queue.Enqueue(agentName, cycleName)
		return
	}

	var fullPrompt string

	if cycle != "" {
		// Agent OS mode: assemble context from DB
		ctx, err := BuildSpawnContext(m.db, project, agentName, cycle, "", ModeHeadless)
		if err != nil {
			m.logger.Error("build context failed", "agent", agentName, "cycle", cycle, "error", err)
			_ = lk.Release()
			return
		}
		fullPrompt = FormatPrompt(ctx)
	} else {
		// Legacy mode: raw prompt
		fullPrompt = prompt
		if fullPrompt == "" {
			fullPrompt = fmt.Sprintf(`You are %s for project %s.

Boot sequence:
1. get_session_context() — full project state
2. get_inbox(unread_only: true, full_content: true) — pending messages

Pass as: "%s" and project: "%s" on EVERY relay tool call.

## Cycle: %s
Process inbox, advance current tasks, update memory before exit.
`, agentName, project, agentName, project, cycleName)
		}
	}

	m.live.Start(agentName+":"+cycleName, cycleName)

	params := SpawnParams{
		Prompt:    fullPrompt,
		TTL:       ttl,
		Streaming: true,
	}

	result := m.executor.RunWithLive(context.Background(), params, m.live.Writer(agentName+":"+cycleName))
	m.live.Finish(agentName + ":" + cycleName)

	// Record metrics
	metric := CycleMetric{
		Agent:               agentName,
		Project:             project,
		Cycle:               cycleName,
		Duration:            result.Duration,
		Success:             result.ExitCode == 0,
		ExitCode:            result.ExitCode,
		Timestamp:           time.Now(),
		InputTokens:         result.Tokens.InputTokens,
		OutputTokens:        result.Tokens.OutputTokens,
		CacheReadTokens:     result.Tokens.CacheReadTokens,
		CacheCreationTokens: result.Tokens.CacheCreationTokens,
	}
	if result.Err != nil {
		metric.Error = result.Err.Error()
	}
	m.metrics.Record(metric)

	// Persist to DB
	errMsg := ""
	if result.Err != nil {
		errMsg = result.Err.Error()
	}
	m.db.RecordCycleHistory(agentName, project, cycleName,
		result.Duration.Milliseconds(), result.ExitCode == 0, result.ExitCode, errMsg,
		result.Tokens.InputTokens, result.Tokens.OutputTokens,
		result.Tokens.CacheReadTokens, result.Tokens.CacheCreationTokens)

	_ = lk.Release()

	// Process queue
	if nextCycleName, ok := m.queue.Dequeue(agentName); ok {
		m.logger.Info("processing queued cycle", "agent", agentName, "cycle", nextCycleName)
		// Look up the schedule for the queued cycle to get its prompt/ttl
		schedules := m.db.ListSchedulesByAgent(project, agentName)
		for _, s := range schedules {
			n, _ := s["name"].(string)
			if n == nextCycleName {
				p, _ := s["prompt"].(string)
				t, _ := s["ttl"].(string)
				c, _ := s["cycle"].(string)
				at, _ := s["allowed_tools"].(string)
				m.executeCycle(agentName, project, nextCycleName, p, t, c, at)
				break
			}
		}
	}
}
