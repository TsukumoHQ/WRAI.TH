export class APIClient {
  constructor(onAgents, onConversations, onNewMessages, onNewTasks, onActivity) {
    this.onAgents = onAgents;
    this.onConversations = onConversations;
    this.onNewMessages = onNewMessages;
    this.onNewTasks = onNewTasks;
    this.onActivity = onActivity;

    this._lastMessageTime = null;
    this._lastTaskTime = null;
    this._agentTimer = null;
    this._msgTimer = null;
    this._convTimer = null;
    this._taskTimer = null;
    this._running = false;
  }

  start() {
    this._running = true;

    // Initial fetch (cross-project)
    this.fetchAllAgents();
    this.fetchAllConversations();
    this.fetchAllTasks().then(tasks => {
      if (this.onNewTasks && tasks.length > 0) this.onNewTasks(tasks);
    });

    // Poll agents every 5s (structural changes only, SSE handles status)
    this._agentTimer = setInterval(() => this.fetchAllAgents(), 5000);

    // Poll conversations every 10s
    this._convTimer = setInterval(() => this.fetchAllConversations(), 10000);

    // Poll new messages every 2s
    this._msgTimer = setInterval(() => this.fetchLatestMessagesAllProjects(), 2000);

    // Poll tasks every 3s
    this._taskTimer = setInterval(() => this.fetchLatestTasks(), 3000);

    // SSE for real-time activity + agent status (<100ms)
    this._sseConnected = false;
    this._activitySource = new EventSource("/api/activity/stream");
    this._activitySource.onopen = () => {
      console.log("[relay] SSE connected");
      this._sseConnected = true;
      // Kill fallback polling if SSE reconnects
      if (this._activityTimer) {
        clearInterval(this._activityTimer);
        this._activityTimer = null;
      }
    };
    this._activitySource.onmessage = (e) => {
      try {
        const payload = JSON.parse(e.data);
        if (payload.sessions && payload.agents) {
          if (this.onActivity) this.onActivity(payload.sessions, payload.agents);
        } else {
          if (this.onActivity) this.onActivity(payload, null);
        }
      } catch (err) {
        console.error("[relay] SSE parse error:", err);
      }
    };
    this._activitySource.onerror = (e) => {
      console.warn("[relay] SSE error, state:", this._activitySource.readyState);
      // Only fallback if SSE is fully closed (readyState === 2)
      if (this._activitySource.readyState === 2 && !this._activityTimer) {
        console.log("[relay] SSE closed, falling back to polling");
        this._activityTimer = setInterval(() => this.fetchActivity(), 1000);
      }
    };
  }

  stop() {
    this._running = false;
    clearInterval(this._agentTimer);
    clearInterval(this._msgTimer);
    clearInterval(this._convTimer);
    clearInterval(this._taskTimer);
    if (this._activitySource) this._activitySource.close();
    clearInterval(this._activityTimer);
  }

  async fetchAllAgents() {
    try {
      const res = await fetch("/api/agents/all");
      if (!res.ok) return;
      const agents = await res.json();
      this.onAgents(agents);
    } catch (e) {
      console.error("[relay] fetchAllAgents error:", e);
    }
  }

  async fetchAllConversations() {
    try {
      const res = await fetch("/api/conversations/all");
      if (!res.ok) return;
      const convs = await res.json();
      this.onConversations(convs);
    } catch (e) {
      console.error("[relay] fetchAllConversations error:", e);
    }
  }

  async fetchLatestMessagesAllProjects() {
    try {
      const since = this._lastMessageTime || new Date(Date.now() - 30000).toISOString();
      const res = await fetch(`/api/messages/latest-all?since=${encodeURIComponent(since)}`);
      if (!res.ok) return;
      const msgs = await res.json();

      if (msgs.length > 0) {
        this._lastMessageTime = msgs[msgs.length - 1].created_at;
        this.onNewMessages(msgs);
      }
    } catch {
      // Silently ignore
    }
  }

  async fetchAllMessagesAllProjects() {
    try {
      const res = await fetch("/api/messages/all-projects");
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async fetchConversationMessages(convId) {
    try {
      const res = await fetch(`/api/conversations/${convId}/messages`);
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async sendUserResponse(project, to, content, replyTo) {
    try {
      const res = await fetch("/api/user-response", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project, to, content, reply_to: replyTo }),
      });
      return res.ok;
    } catch {
      return false;
    }
  }

  async fetchActivity() {
    try {
      const res = await fetch("/api/activity");
      if (!res.ok) return;
      const sessions = await res.json();
      if (this.onActivity) this.onActivity(sessions);
    } catch {
      // Silently ignore
    }
  }

  // --- Memory API ---

  async fetchMemories(params = {}) {
    try {
      const qs = new URLSearchParams();
      if (params.project) qs.set("project", params.project);
      if (params.scope) qs.set("scope", params.scope);
      if (params.agent) qs.set("agent", params.agent);
      if (params.tag) qs.set("tag", params.tag);
      const res = await fetch(`/api/memories?${qs}`);
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async searchMemories(query) {
    try {
      const res = await fetch(`/api/memories/search?q=${encodeURIComponent(query)}`);
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async createMemory(data) {
    try {
      const res = await fetch("/api/memories", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(data),
      });
      return res.ok ? await res.json() : null;
    } catch {
      return null;
    }
  }

  async deleteMemory(id) {
    try {
      const res = await fetch(`/api/memories/${id}`, { method: "DELETE" });
      return res.ok;
    } catch {
      return false;
    }
  }

  async resolveConflict(key, chosenValue, project, scope) {
    try {
      const res = await fetch(`/api/memories/${encodeURIComponent(key)}/resolve`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ chosen_value: chosenValue, project, scope }),
      });
      return res.ok ? await res.json() : null;
    } catch {
      return null;
    }
  }

  // --- Task API ---

  async fetchAllTasks() {
    try {
      const res = await fetch("/api/tasks/all");
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async fetchTasks(params = {}) {
    try {
      const qs = new URLSearchParams();
      if (params.project) qs.set("project", params.project);
      if (params.status) qs.set("status", params.status);
      if (params.profile) qs.set("profile", params.profile);
      if (params.priority) qs.set("priority", params.priority);
      const res = await fetch(`/api/tasks?${qs}`);
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async fetchLatestTasks() {
    try {
      const since = this._lastTaskTime || new Date(Date.now() - 30000).toISOString();
      const res = await fetch(`/api/tasks/latest?since=${encodeURIComponent(since)}`);
      if (!res.ok) return;
      const tasks = await res.json();
      if (tasks.length > 0) {
        this._lastTaskTime = tasks[tasks.length - 1].dispatched_at;
        if (this.onNewTasks) this.onNewTasks(tasks);
      }
    } catch {
      // Silently ignore
    }
  }

  async dispatchTask(data) {
    try {
      const res = await fetch("/api/tasks", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(data),
      });
      if (!res.ok) return null;
      return await res.json();
    } catch {
      return null;
    }
  }

  async transitionTask(taskId, status, project, agent, result, reason) {
    try {
      const body = { status, project: project || "default", agent: agent || "user" };
      if (result) body.result = result;
      if (reason) body.reason = reason;
      const res = await fetch(`/api/tasks/${taskId}/transition`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      if (!res.ok) return null;
      return await res.json();
    } catch {
      return null;
    }
  }

  async cancelTask(taskId, project, agent) {
    return this.transitionTask(taskId, "cancelled", project, agent);
  }

  async fetchAllTeams() {
    try {
      const res = await fetch("/api/teams/all");
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async fetchBoards(project) {
    try {
      const res = await fetch(`/api/boards?project=${encodeURIComponent(project)}`);
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async fetchAllBoards() {
    try {
      const res = await fetch("/api/boards/all");
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async updateTask(taskId, data) {
    try {
      const res = await fetch(`/api/tasks/${taskId}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(data),
      });
      if (!res.ok) return null;
      return await res.json();
    } catch {
      return null;
    }
  }

  async deleteTask(taskId, project) {
    try {
      const qs = project ? `?project=${encodeURIComponent(project)}` : "";
      const res = await fetch(`/api/tasks/${taskId}${qs}`, { method: "DELETE" });
      return res.ok;
    } catch {
      return false;
    }
  }

  async fetchTask(taskId, project) {
    try {
      const qs = project ? `?project=${encodeURIComponent(project)}` : "";
      const res = await fetch(`/api/tasks/${taskId}${qs}`);
      if (!res.ok) return null;
      return await res.json();
    } catch {
      return null;
    }
  }

  // --- Kanban board (mirror read-replica) ---

  // One call, all board tasks for a project (optionally a single cycle). Zero
  // Linear round-trips. cycle: "active" | "all" | cycle_id | "".
  async fetchBoardTasks(project, cycle) {
    try {
      const qs = new URLSearchParams();
      if (project) qs.set("project", project);
      if (cycle) qs.set("cycle", cycle);
      const res = await fetch(`/api/tasks/board?${qs}`);
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async fetchCycles(project) {
    try {
      const qs = project ? `?project=${encodeURIComponent(project)}` : "";
      const res = await fetch(`/api/cycles${qs}`);
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async fetchTaskProgress(taskId, project) {
    try {
      const qs = project ? `?project=${encodeURIComponent(project)}` : "";
      const res = await fetch(`/api/tasks/${taskId}/progress${qs}`);
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  // Subscribe to the semantic task lifecycle stream (/api/events/stream).
  // onTaskEvent receives the parsed MCPEvent for any task.* event. Returns the
  // EventSource so the caller can close it.
  subscribeTaskEvents(onTaskEvent) {
    let src;
    try {
      src = new EventSource("/api/events/stream");
    } catch {
      return null;
    }
    src.onmessage = (e) => {
      try {
        const evt = JSON.parse(e.data);
        if (evt && typeof evt.type === "string" && evt.type.startsWith("task.")) {
          onTaskEvent(evt);
        }
      } catch {
        // ignore malformed frames
      }
    };
    src.onerror = () => { /* browser auto-reconnects EventSource */ };
    this._eventsSource = src;
    return src;
  }

  // --- Profiles API ---

  async fetchProfiles(project) {
    try {
      const qs = project ? `?project=${encodeURIComponent(project)}` : '';
      const res = await fetch(`/api/profiles${qs}`);
      if (!res.ok) return [];
      return await res.json();
    } catch { return []; }
  }

  async fetchProfile(slug, project) {
    try {
      const qs = project ? `?project=${encodeURIComponent(project)}` : '';
      const res = await fetch(`/api/profiles/${encodeURIComponent(slug)}${qs}`);
      if (!res.ok) return null;
      return await res.json();
    } catch { return null; }
  }


  // --- Projects API ---

  async fetchProjects() {
    try {
      const res = await fetch("/api/projects");
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async fetchProject(name) {
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(name)}`);
      if (!res.ok) return null;
      return await res.json();
    } catch {
      return null;
    }
  }

  async updateProjectPlanet(name, planetType) {
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(name)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ planet_type: planetType }),
      });
      return res.ok;
    } catch { return false; }
  }

  async fetchSettings() {
    try {
      const res = await fetch("/api/settings");
      if (!res.ok) return {};
      return await res.json();
    } catch { return {}; }
  }

  async updateSettings(settings) {
    try {
      const res = await fetch("/api/settings", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(settings),
      });
      return res.ok;
    } catch { return false; }
  }

  async fetchFileLocks(project) {
    try {
      const qs = project ? `?project=${encodeURIComponent(project)}` : "";
      const res = await fetch(`/api/file-locks${qs}`);
      if (!res.ok) return [];
      return await res.json();
    } catch {
      return [];
    }
  }

  async fetchTokenUsage(period = "24h") {
    try {
      const res = await fetch(`/api/token-usage?period=${encodeURIComponent(period)}`);
      if (!res.ok) return [];
      return await res.json();
    } catch { return []; }
  }

  async fetchTokenUsageByProject(project, period = "24h") {
    try {
      const res = await fetch(`/api/token-usage/project?project=${encodeURIComponent(project)}&period=${encodeURIComponent(period)}`);
      if (!res.ok) return [];
      return await res.json();
    } catch { return []; }
  }

  async fetchTokenUsageByAgent(project, agent, period = "24h") {
    try {
      const qs = `project=${encodeURIComponent(project)}&period=${encodeURIComponent(period)}`;
      const agentQs = agent ? `&agent=${encodeURIComponent(agent)}` : "";
      const res = await fetch(`/api/token-usage/agent?${qs}${agentQs}`);
      if (!res.ok) return [];
      return await res.json();
    } catch { return []; }
  }

  async fetchTokenTimeSeries(project, period = "24h", agent = "") {
    try {
      let qs = `project=${encodeURIComponent(project)}&period=${encodeURIComponent(period)}`;
      if (agent) qs += `&agent=${encodeURIComponent(agent)}`;
      const res = await fetch(`/api/token-usage/timeseries?${qs}`);
      if (!res.ok) return [];
      return await res.json();
    } catch { return []; }
  }
}
