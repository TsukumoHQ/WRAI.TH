import { CanvasEngine } from "./canvas.js";
import { SpaceBackground, World } from "./world.js";
import { AgentView } from "./agent-view.js";
import { APIClient } from "./api-client.js";
import { MessageOrb } from "./message-orb.js";
import { KanbanBoard } from "./kanban.js";
import { StatsPanel } from "./stats.js";
import { NotificationsPanel } from "./notifications.js";
import { CommandPanel } from "./command-panel.js";
import { ShortcutManager } from "./shortcuts.js";
import { ConnectionOverlay } from "./connections.js";
import { MCPEffects } from "./mcp-effects.js";
import { spaceAssets } from "./space-assets.js";
import { roboSprite } from "./robo-sprite.js";
import { mechSprite } from "./mech-sprite.js";

// --- Composite key for cross-project agent identity ---
function agentKey(project, name) {
  return `${project}:${name}`;
}

// DOM elements
const canvas = document.getElementById("relay-canvas");
const statusDot = document.getElementById("status-dot");
const agentCountEl = document.getElementById("agent-count");
const messagesTitle = document.getElementById("messages-title");
const messagesList = document.getElementById("messages-list");
const detailPanel = document.getElementById("agent-detail");
const detailName = document.getElementById("detail-name");
const detailRole = document.getElementById("detail-role");
const detailDesc = document.getElementById("detail-desc");
const detailProject = document.getElementById("detail-project");
const detailStatus = document.getElementById("detail-status");
const detailLastSeen = document.getElementById("detail-last-seen");
const detailRegistered = document.getElementById("detail-registered");
const detailClose = document.getElementById("detail-close");
const detailReportsTo = document.getElementById("detail-reports-to");
const detailDirectReports = document.getElementById("detail-direct-reports");
const userQuestionsPanel = document.getElementById("user-questions");

// Preload space assets
spaceAssets.preload();
roboSprite.preload();
mechSprite.preload();

// State
const engine = new CanvasEngine(canvas);
const worldBg = new SpaceBackground();
const world = new World();
const agentViews = new Map();      // "project:name" -> AgentView
let projectGroups = new Map();      // project -> Set<agentKey>
let conversations = [];             // cached conversation list
let focusedAgent = null;            // "project:name" of focused agent, or null
let focusedProject = null;          // project name when zoomed into a cluster, or null
let focusedTeam = null;             // { project, slug, members: [agentName] } when viewing a team
let paletteCounter = 0;
let agentsData = [];                // cached raw agent data for hierarchy
let teamsData = [];                 // cached teams with members
let connected = false;
let firstLayout = true;
let hoveredAgentKey = null;
let activitySessions = {};

const connectionOverlay = new ConnectionOverlay();
const mcpEffects = new MCPEffects();

engine.add(worldBg);
engine.add(connectionOverlay);
engine.add(world);
engine.add(mcpEffects);
engine.start();

// MCP effects: resolve agent positions for animations
mcpEffects.setAgentResolver((project, name) => {
  const key = agentKey(project, name);
  const av = agentViews.get(key);
  if (!av) return null;
  return { x: av.x, y: av.y };
});
mcpEffects.connect();

// --- Cluster layout ---

let _teleportAgents = false; // skip lerp, snap to position

function layoutAgents() {
  const projects = [...projectGroups.keys()].sort();
  const count = agentViews.size;
  if (count === 0 && viewMode !== "galaxy") {
    world.clusters = [];
    return;
  }
  // In galaxy view, we may have projects even with no local agents
  if (viewMode === "galaxy" && count === 0 && projectsData.length === 0) {
    world.clusters = [];
    return;
  }

  // World-space origin
  const cx = engine.width / 2;
  const cy = engine.height / 2;

  // Clear state (preserve projectPlanets for angle persistence)
  const _prevPlanets = world.projectPlanets;
  world.sunCenter = null;
  world.projectPlanets = [];
  world.colony = null;

  for (const [, av] of agentViews) {
    av.minimal = viewMode === "galaxy";
    if (viewMode !== "galaxy") av.orbit = null;
  }

  // Note: no projectGroups.has() guard — a freshly created project has zero
  // agents but must still render its (empty) colony surface, not the galaxy.
  if (viewMode === "colony" && colonyProject) {
    // --- Colony view: focused project, agents on planet surface ---
    const project = colonyProject;
    const keys = [...(projectGroups.get(project) || new Set())];
    const agentCount = keys.length;

    // Find planet type from projectsData (DB) -> derive solarPlanet for biome
    const projInfo = projectsData.find(p => p.name === project);
    const planetType = projInfo ? projInfo.planet_type : "terran/1";
    const biomeCategory = planetType.split("/")[0]; // e.g. "terran", "lava"
    // Map biome category to a solar planet for surface rendering
    const BIOME_TO_SOLAR = {
      barren: "mercury", desert: "mars", forest: "earth", gas_giant: "jupiter",
      ice: "uranus", lava: "venus", ocean: "neptune", terran: "earth", tundra: "uranus",
    };
    const solarPlanet = BIOME_TO_SOLAR[biomeCategory] || "earth";

    // Build hierarchy tree for colony layout
    const children = new Map();
    const roots = [];
    for (const key of keys) {
      const av = agentViews.get(key);
      if (!av) continue;
      if (av._reportsTo) {
        const parentKey = agentKey(av.project, av._reportsTo);
        if (keys.includes(parentKey)) {
          if (!children.has(parentKey)) children.set(parentKey, []);
          children.get(parentKey).push(key);
        } else {
          roots.push(key);
        }
      } else {
        roots.push(key);
      }
    }

    function setColonyMode(av) {
      av.orbit = null;
      av.solarPlanet = null;
      av.minimal = false;
      av.colony = true; // mech sprites in colony view
    }

    // If no hierarchy, simple horizontal layout centered
    if (roots.length === agentCount || agentCount <= 2) {
      const AGENT_SPACING = Math.min(180, engine.width / (agentCount + 1));
      const startX = cx - (agentCount - 1) * AGENT_SPACING / 2;
      for (let i = 0; i < keys.length; i++) {
        const av = agentViews.get(keys[i]);
        if (!av) continue;
        setColonyMode(av);
        av.targetX = agentCount === 1 ? cx : startX + i * AGENT_SPACING;
        av.targetY = cy;
      }
    } else {
      // Tree layout: compute depth, center vertically
      const subtreeWidth = new Map();
      const subtreeDepth = new Map();
      function computeColonyTree(key) {
        const kids = children.get(key) || [];
        if (kids.length === 0) {
          subtreeWidth.set(key, 1);
          subtreeDepth.set(key, 0);
          return;
        }
        let w = 0, maxD = 0;
        for (const k of kids) {
          computeColonyTree(k);
          w += subtreeWidth.get(k) || 1;
          maxD = Math.max(maxD, (subtreeDepth.get(k) || 0) + 1);
        }
        subtreeWidth.set(key, Math.max(w, 1));
        subtreeDepth.set(key, maxD);
      }
      let totalRootWidth = 0;
      let maxTreeDepth = 0;
      for (const r of roots) {
        computeColonyTree(r);
        totalRootWidth += subtreeWidth.get(r) || 1;
        maxTreeDepth = Math.max(maxTreeDepth, (subtreeDepth.get(r) || 0) + 1);
      }

      const V_SPACING = Math.min(140, (engine.height * 0.7) / Math.max(maxTreeDepth, 1));
      const H_SPACING = Math.min(180, (engine.width * 0.85) / Math.max(totalRootWidth, 1));

      function placeColonySubtree(key, left, top) {
        const w = subtreeWidth.get(key) || 1;
        const av = agentViews.get(key);
        if (av) {
          setColonyMode(av);
          av.targetX = left + (w * H_SPACING) / 2;
          av.targetY = top;
        }
        const kids = children.get(key) || [];
        let childLeft = left;
        for (const k of kids) {
          const kw = subtreeWidth.get(k) || 1;
          placeColonySubtree(k, childLeft, top + V_SPACING);
          childLeft += kw * H_SPACING;
        }
      }

      const totalW = totalRootWidth * H_SPACING;
      const totalH = maxTreeDepth * V_SPACING;
      let startX = cx - totalW / 2;
      const startY = cy - totalH / 2;

      for (const r of roots) {
        const rw = subtreeWidth.get(r) || 1;
        placeColonySubtree(r, startX, startY);
        startX += rw * H_SPACING;
      }
    }

    // Hide other project agents off-screen
    for (const [key, av] of agentViews) {
      if (!keys.includes(key)) {
        av.orbit = null;
        av.colony = false;
        av.targetX = -9999;
        av.targetY = -9999;
      }
    }

    // Store planet type on world for colony rendering (planet in corner)
    world.clusters = [];
    world.colony = { project, solarPlanet, planetType };
    world.sunCenter = null;
    world.projectPlanets = [];
    engine.camera.snapTo(cx, cy, 1.0);

  } else {
    // --- Galaxy view: planets representing projects (no sun, no agents visible) ---

    // Hide ALL agents in galaxy view
    for (const [, av] of agentViews) {
      av.orbit = null;
      av.solarPlanet = null;
      av.colony = false;
      av.targetX = -9999;
      av.targetY = -9999;
      av.minimal = true;
    }

    // Layout project planets — preserve existing planet state (angles, orbits)
    const projectList = projectsData.length > 0
      ? projectsData
      : projects.map(p => ({ name: p, planet_type: "terran/1", agent_count: (projectGroups.get(p) || new Set()).size }));

    // Build a lookup of previous planets to preserve their orbit angle
    const existingByName = new Map();
    for (const ep of _prevPlanets) {
      existingByName.set(ep.project, ep);
    }

    const projectPlanets = [];

    // Planet size: 32-64px (max 80% of 80px sun)
    const planetSize = (count) => Math.min(32 + Math.min(count * 3, 32), 64);

    if (projectList.length === 1) {
      const p = projectList[0];
      const agentCount = p.agent_count || (projectGroups.get(p.name) || new Set()).size;
      const existing = existingByName.get(p.name);
      const orbitR = Math.min(engine.width, engine.height) * 0.25;
      projectPlanets.push({
        project: p.name,
        planetType: p.planet_type || "terran/1",
        orbitRadius: orbitR,
        angle: existing ? existing.angle : 0,
        speed: 0.03,
        cx: cx + Math.cos(existing ? existing.angle : 0) * orbitR,
        cy: cy + Math.sin(existing ? existing.angle : 0) * orbitR,
        agentCount,
        size: planetSize(agentCount),
      });
    } else {
      // Distribute planets across 2-3 distinct orbital rings
      // Sort by agent count — biggest planets on OUTER ring (more space)
      const sorted = [...projectList].sort((a, b) => {
        const ac = a.agent_count || (projectGroups.get(a.name) || new Set()).size;
        const bc = b.agent_count || (projectGroups.get(b.name) || new Set()).size;
        return ac - bc; // smallest first → inner ring, biggest → outer
      });
      const outerRadius = Math.min(engine.width, engine.height) * 0.36;
      const n = sorted.length;
      // Split into rings: up to 5 per ring
      const maxPerRing = Math.max(4, Math.ceil(n / 3));
      const rings = [];
      for (let i = 0; i < n; i += maxPerRing) {
        rings.push(sorted.slice(i, i + maxPerRing));
      }
      // Well-spaced radii with room for asteroid belt between
      const ringRadii = rings.length === 1
        ? [0.65]
        : rings.length === 2
        ? [0.45, 0.82]  // asteroid belt at ~0.63
        : [0.38, 0.62, 0.88]; // asteroid belt between ring 1 & 2

      for (let r = 0; r < rings.length; r++) {
        const ring = rings[r];
        const orbitR = outerRadius * ringRadii[r];
        const speed = 0.025 / Math.sqrt(ringRadii[r]);
        // Golden angle offset per ring so rings don't align
        const ringOffset = r * 0.85;
        for (let j = 0; j < ring.length; j++) {
          const p = ring[j];
          const agentCount = p.agent_count || (projectGroups.get(p.name) || new Set()).size;
          const existing = existingByName.get(p.name);
          const startAngle = ringOffset + (j / ring.length) * Math.PI * 2;
          const angle = existing ? existing.angle : startAngle;

          projectPlanets.push({
            project: p.name,
            planetType: p.planet_type || "terran/1",
            orbitRadius: orbitR,
            angle,
            speed: speed * (0.9 + Math.random() * 0.2), // slight speed variation
            cx: cx + Math.cos(angle) * orbitR,
            cy: cy + Math.sin(angle) * orbitR,
            agentCount,
            size: planetSize(agentCount),
          });
        }
      }
    }

    world.sunCenter = { cx, cy };
    world.projectPlanets = projectPlanets;
    world.clusters = [];
    world.colony = null;


    // Pre-populate project stats for progress bars
    for (const p of projectsData) {
      world._projectStats[p.name] = {
        total: p.agent_count, online: p.online_count,
        tasks: p.total_tasks, active: p.active_tasks, done: p.done_tasks,
        tokens_24h: p.tokens_24h || 0,
      };
    }

    engine.camera.snapTo(cx, cy, 1.0);
  }

  // Teleport: snap agents to their target positions (skip lerp)
  if (_teleportAgents) {
    _teleportAgents = false;
    for (const [, av] of agentViews) {
      av.x = av.targetX;
      av.y = av.targetY;
    }
  }
}

/** Smoothly fit camera to show a single cluster (multi-project zoom). */
function fitToCluster(cluster) {
  const diam = (cluster.radius + 40) * 2;
  const zoomX = (engine.width * 0.8) / diam;
  const zoomY = (engine.height * 0.8) / diam;
  const zoom = Math.max(0.15, Math.min(zoomX, zoomY));

  if (firstLayout) {
    engine.camera.snapTo(cluster.cx, cluster.cy, zoom);
    firstLayout = false;
  } else {
    engine.camera.lookAt(cluster.cx, cluster.cy, zoom);
  }
}

/** Smoothly fit camera to show all clusters (multi-project overview). */
function fitToAllClusters() {
  const clusters = world.clusters;
  if (clusters.length === 0) return;

  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
  for (const c of clusters) {
    minX = Math.min(minX, c.cx - c.radius);
    minY = Math.min(minY, c.cy - c.radius);
    maxX = Math.max(maxX, c.cx + c.radius);
    maxY = Math.max(maxY, c.cy + c.radius);
  }

  const contentW = maxX - minX || 1;
  const contentH = maxY - minY || 1;
  const centerX = (minX + maxX) / 2;
  const centerY = (minY + maxY) / 2;
  const zoomX = (engine.width * 0.85) / contentW;
  const zoomY = (engine.height * 0.85) / contentH;
  const zoom = Math.max(0.15, Math.min(zoomX, zoomY));

  if (firstLayout) {
    engine.camera.snapTo(centerX, centerY, zoom);
    firstLayout = false;
  } else {
    engine.camera.lookAt(centerX, centerY, zoom);
  }
}

// --- API callbacks ---

function onAgents(agents) {
  if (!connected) {
    connected = true;
    statusDot.classList.add("connected");
  }

  agentCountEl.textContent = `${agents.length} agent${agents.length !== 1 ? "s" : ""}`;

  const currentKeys = new Set(agents.map(a => agentKey(a.project || "default", a.name)));

  // Detect structural changes (agents added/removed/changed project)
  let structureChanged = false;
  for (const key of currentKeys) {
    if (!agentViews.has(key)) { structureChanged = true; break; }
  }
  if (!structureChanged) {
    for (const key of agentViews.keys()) {
      if (!currentKeys.has(key)) { structureChanged = true; break; }
    }
  }

  // Remove agents that no longer exist
  for (const [key, av] of agentViews) {
    if (!currentKeys.has(key)) {
      engine.remove(av);
      agentViews.delete(key);
    }
  }

  // Rebuild project groups
  projectGroups = new Map();

  // Add/update agents
  for (const a of agents) {
    const project = a.project || "default";
    const key = agentKey(project, a.name);

    // Track project groups
    if (!projectGroups.has(project)) projectGroups.set(project, new Set());
    projectGroups.get(project).add(key);

    let av = agentViews.get(key);
    if (!av) {
      av = new AgentView(a.name, a.role, a.description, paletteCounter++, a.online, project);
      av.setPosition(engine.width / 2, engine.height / 2);
      av.spawnEffect();
      agentViews.set(key, av);
      engine.add(av);
    } else {
      av.online = a.online;
      av.role = a.role;
      av.description = a.description;
    }
    av._reportsTo = a.reports_to || null;
    av._lastSeenRaw = a.last_seen;
    av._registeredRaw = a.registered_at;
    av.isExecutive = a.is_executive || false;
    av._teams = a.teams || [];
    av.session_id = a.session_id || null;
    av.sleeping = a.status === "sleeping";
    // Planet type now comes from project, not agent
    // Apply activity from agents API (enriched by ingester)
    if (a.activity && a.activity !== "idle") {
      av.activity = a.activity;
      av.activityTool = a.activity_tool || "";
    }
  }

  // Update agent task labels from current tasks
  updateAgentTaskLabels();

  // Update connection overlay with current teams
  updateConnectionOverlay();

  agentsData = agents;

  // In colony view, show project tags only if somehow multiple projects
  // In galaxy view, agents are hidden (off-screen), no need for tags
  for (const [, av] of agentViews) {
    av.showProjectTag = false;
    av.minimal = viewMode === "galaxy";
  }

  // Only re-layout if agents were added/removed — preserve orbits on routine polls
  if (structureChanged || firstLayout) {
    layoutAgents();
    updateHierarchyLinks();
  }
  updateHighlights();
}

function onActivity(sessions, sseAgents) {
  activitySessions = sessions;

  // If we have enriched agent data from SSE, apply statuses directly
  if (sseAgents) {
    for (const sa of sseAgents) {
      const key = agentKey(sa.project, sa.name);
      const av = agentViews.get(key);
      if (!av) continue;

      // Apply status
      av.sleeping = sa.status === "sleeping";
      av.online = sa.status === "busy" || sa.status === "active";
      av._sseStatus = sa.status; // busy, active, sleeping, inactive, deleted

      // Apply activity
      if (sa.activity && sa.activity !== "idle") {
        applyActivity(av, { activity: sa.activity, tool: sa.activity_tool, file: "" });
      } else {
        av.activity = null;
        av.activityTool = "";
      }
    }
  }

  // Handle ghost sprites for unmatched sessions
  const matchedSessionIDs = new Set();
  if (sseAgents) {
    for (const sa of sseAgents) {
      if (sa.session_id) matchedSessionIDs.add(sa.session_id);
    }
  }

  // Ghost sprites disabled — only registered agents appear on canvas
}

const ACTIVITY_GLOW = {
  reading: "#74b9ff",
  writing: "#a29bfe",
  thinking: "#ffd93d",
  tool_use: "#55efc4",
  executing: "#fd79a8",
};

function applyActivity(av, s) {
  const prevActivity = av.activity;
  av.activity = s.activity || null;
  av.activityTool = s.tool || "";
  av.activityFile = s.file || "";
  if (prevActivity !== av.activity && av.activity && av.activity !== "idle") {
    const glowColor = ACTIVITY_GLOW[av.activity];
    if (glowColor && av.particles) av.particles.emitActivity(av.x, av.y + 24, glowColor);
  }
}

function onConversations(convs) {
  conversations = convs;
  updateConvFilterOptions();
}

function onNewMessages(msgs) {
  checkForUserMessages(msgs);

  for (const msg of msgs) {
    const msgProject = msg.project || "default";
    const fromKey = agentKey(msgProject, msg.from);
    const fromAv = agentViews.get(fromKey);

    if (fromAv) {
      const preview = msg.subject || msg.content.slice(0, 80);
      fromAv.showBubble(preview, "speech");
    }

    const msgKind = msg.type || "default";

    if (fromAv && msg.to && msg.to.startsWith("team:")) {
      // Team-addressed message — send orbs to all team members
      const teamSlug = msg.to.slice(5);
      const teamMembers = getTeamMemberKeys(msgProject, teamSlug);
      for (const memberKey of teamMembers) {
        if (memberKey !== fromKey) {
          const targetAv = agentViews.get(memberKey);
          if (targetAv) {
            const orb = new MessageOrb(
              fromAv.x, fromAv.y,
              targetAv.x, targetAv.y,
              msgKind,
              () => { engine.remove(orb); targetAv.arrivalBurst(msgKind); },
              msg.priority
            );
            engine.add(orb);
          }
        }
      }
    } else if (fromAv && msg.to && msg.to !== "*") {
      const toKey = agentKey(msgProject, msg.to);
      const toAv = agentViews.get(toKey);
      if (toAv) {
        const orb = new MessageOrb(
          fromAv.x, fromAv.y,
          toAv.x, toAv.y,
          msgKind,
          () => { engine.remove(orb); toAv.arrivalBurst(msgKind); },
          msg.priority
        );
        engine.add(orb);
      }
    } else if (fromAv && msg.to === "*") {
      for (const [key, av] of agentViews) {
        if (key !== fromKey) {
          const orb = new MessageOrb(
            fromAv.x, fromAv.y,
            av.x, av.y,
            msg.type || "notification",
            () => { engine.remove(orb); av.arrivalBurst(msg.type || "notification"); },
            msg.priority
          );
          engine.add(orb);
        }
      }
    } else if (fromAv && msg.conversation_id) {
      const conv = conversations.find(c => c.id === msg.conversation_id);
      if (conv && conv.members) {
        for (const member of conv.members) {
          if (member !== msg.from) {
            const targetKey = agentKey(msgProject, member);
            const targetAv = agentViews.get(targetKey);
            if (targetAv) {
              const orb = new MessageOrb(
                fromAv.x, fromAv.y,
                targetAv.x, targetAv.y,
                msgKind,
                () => { engine.remove(orb); targetAv.arrivalBurst(msgKind); },
                msg.priority
              );
              engine.add(orb);
            }
          }
        }
      }
    }

    // Append to messages panel respecting focus context (with typewriter for live)
    if (currentMode !== "kanban") {
      let show = false;
      if (focusedAgent) {
        const focusAv = agentViews.get(focusedAgent);
        show = focusAv && msgProject === focusAv.project &&
            (msg.from === focusAv.name || msg.to === focusAv.name);
      } else if (focusedTeam) {
        const memberSet = new Set(focusedTeam.members);
        show = msgProject === focusedTeam.project && (memberSet.has(msg.from) || memberSet.has(msg.to));
      } else if (focusedProject) {
        show = msgProject === focusedProject;
      } else {
        show = true;
      }
      if (show) appendMessage(msg, false, true);
    }
  }
}

// --- Focus / Highlights ---

function updateHighlights() {
  const convId = msgConvFilter ? msgConvFilter.value : "";

  if (!convId) {
    // No filter — show all
    for (const [, av] of agentViews) {
      av.highlighted = true;
      av.dimMode = false;
    }
    return;
  }

  // Find conversation members
  const conv = conversations.find(c => c.id === convId);
  const members = conv ? (conv.members || []) : [];
  const memberSet = new Set(members);

  for (const [key, av] of agentViews) {
    const inConv = memberSet.has(av.name);
    av.highlighted = inConv;
    av.dimMode = !inConv;
  }
}

/** Filter messages by current view context. */
function filterMessagesByView(msgs) {
  if (focusedAgent) {
    const av = agentViews.get(focusedAgent);
    if (!av) return [];
    messagesTitle.textContent = av.name;
    return msgs.filter(m => {
      const mp = m.project || "default";
      return mp === av.project && (m.from === av.name || m.to === av.name);
    });
  } else if (focusedTeam) {
    messagesTitle.textContent = `team: ${focusedTeam.slug}`;
    const memberSet = new Set(focusedTeam.members);
    return msgs.filter(m => {
      const mp = m.project || "default";
      return mp === focusedTeam.project && (memberSet.has(m.from) || memberSet.has(m.to));
    });
  } else if (focusedProject) {
    messagesTitle.textContent = focusedProject;
    return msgs.filter(m => (m.project || "default") === focusedProject);
  } else {
    messagesTitle.textContent = "All Messages";
    return msgs;
  }
}

async function loadMessages() {
  const allMsgs = await client.fetchAllMessagesAllProjects();
  const filtered = filterMessagesByView(allMsgs);

  // Build set of wanted msg IDs
  const wantedIds = new Set(filtered.map(m => m.id));

  // Remove stale items
  messagesList.querySelectorAll(".msg-item[data-msg-id]").forEach(el => {
    if (!wantedIds.has(el.dataset.msgId)) el.remove();
  });

  // Track existing
  const existingIds = new Set();
  messagesList.querySelectorAll(".msg-item[data-msg-id]").forEach(el => {
    existingIds.add(el.dataset.msgId);
  });

  // Remove empty placeholder
  const empty = messagesList.querySelector(".msg-empty");
  if (empty) empty.remove();

  if (filtered.length === 0) {
    messagesList.innerHTML = '<div class="msg-empty">No messages yet</div>';
    return;
  }

  for (const msg of filtered) {
    if (!existingIds.has(msg.id)) {
      appendMessage(msg);
    }
  }
  messagesList.scrollTop = messagesList.scrollHeight;
}

function appendMessage(msg, showConv = false, useTypewriter = false) {
  const el = document.createElement("div");
  const priority = msg.priority || "P2";
  el.className = `msg-item priority-${priority}`;
  el.dataset.msgId = msg.id;
  if (msg.conversation_id) el.dataset.convId = msg.conversation_id;

  const time = formatTime(msg.created_at);
  const subject = msg.subject ? `<span class="msg-subject">${escapeHtml(msg.subject)}</span>` : "";
  const content = msg.content.length > 500 ? msg.content.slice(0, 497) + "..." : msg.content;

  // Priority tag for P0/P1
  let priorityTag = "";
  if (priority === "P0") {
    priorityTag = `<span class="msg-priority-tag P0">P0</span>`;
  } else if (priority === "P1") {
    priorityTag = `<span class="msg-priority-tag P1">P1</span>`;
  }

  // Delivery state badge
  let deliveryBadge = "";
  if (msg.delivery_state) {
    deliveryBadge = `<span class="msg-delivery-badge state-${msg.delivery_state}">${msg.delivery_state}</span>`;
  }

  let convTag = "";
  if (msg.conversation_id) {
    const conv = conversations.find(c => c.id === msg.conversation_id);
    const convName = conv ? conv.title : "conv";
    convTag = `<span class="msg-conv-tag">${escapeHtml(convName)}</span> `;
  }

  // Show project tag for cross-project view
  let projectTag = "";
  const msgProject = msg.project || "default";
  if (projectGroups.size > 1 && msgProject !== "default") {
    projectTag = `<span class="msg-conv-tag">${escapeHtml(msgProject)}</span> `;
  }

  // Team-addressed messages get a team tag
  let toTag = "";
  if (msg.to && msg.to.startsWith("team:")) {
    const teamSlug = msg.to.slice(5);
    toTag = `<span class="msg-team-tag">→ team:${escapeHtml(teamSlug)}</span> `;
  } else if (msg.to && msg.to === "*") {
    toTag = `<span class="msg-team-tag">→ broadcast</span> `;
  } else if (msg.to) {
    toTag = `<span class="msg-to">→ ${escapeHtml(msg.to)}</span> `;
  }

  el.innerHTML = `
    ${subject}
    ${priorityTag}${projectTag}${convTag}<span class="msg-from">${escapeHtml(msg.from)}</span> ${toTag}${deliveryBadge}
    <span class="msg-content">${useTypewriter ? "" : escapeHtml(content)}</span>
    <div class="msg-time">${time}</div>
  `;

  messagesList.appendChild(el);
  messagesList.scrollTop = messagesList.scrollHeight;

  // Typewriter effect for new live messages
  if (useTypewriter && content.length > 0) {
    typewriterAppend(el, content, 12);
  }
}

// --- Hierarchy links ---

function updateHierarchyLinks() {
  // Hide hierarchy lines in galaxy view
  connectionOverlay.showHierarchy = viewMode === "colony";
  if (viewMode === "galaxy") {
    world.hierarchyLinks = [];
    return;
  }
  const links = [];
  for (const [, av] of agentViews) {
    if (av._reportsTo) {
      const managerKey = agentKey(av.project, av._reportsTo);
      const managerAv = agentViews.get(managerKey);
      if (managerAv) {
        links.push({ from: managerAv, to: av });
      }
    }
  }
  world.hierarchyLinks = links;
}

// --- User notification inbox ---
// Catches all messages sent to "user" (questions, notifications, responses, tasks, etc.)

const shownUserMsgs = new Set();
const _celebratedTasks = new Set();

function checkForUserMessages(msgs) {
  for (const msg of msgs) {
    if (shownUserMsgs.has(msg.id)) continue;
    // Show if explicitly to "user" or if type is user_question
    const isForUser = (msg.to === "user") || (msg.type === "user_question");
    if (!isForUser) continue;
    shownUserMsgs.add(msg.id);
    showUserCard(msg);
  }
}

function userMsgCategory(msg) {
  const t = msg.type || "";
  if (t === "user_question" || t === "question") return "question";
  if (t === "notification") return "notification";
  if (t === "task") return "task";
  return "response"; // response, code-snippet, or any other
}

function userMsgTypeLabel(cat) {
  switch (cat) {
    case "question": return "Question";
    case "notification": return "Notification";
    case "task": return "Task";
    default: return "Response";
  }
}

const UQ_ICONS = {
  question: "/img/ui/icons/large/chromatic_aberration/questionmark.png",
  notification: "/img/ui/icons/large/chromatic_aberration/exclamation.png",
  response: "/img/ui/icons/large/chromatic_aberration/thumbs_up.png",
  task: "/img/ui/icons/large/chromatic_aberration/interrobang.png",
};

function _uqLoadingWheel() {
  const el = document.createElement("span");
  el.className = "uq-loading-wheel";
  let frame = 1;
  const img = document.createElement("img");
  img.src = `/img/ui/loading_wheel/loading_whee1.png`;
  img.width = 16; img.height = 16;
  el.appendChild(img);
  el._interval = setInterval(() => {
    frame = (frame % 11) + 1;
    img.src = `/img/ui/loading_wheel/loading_whee${frame}.png`;
  }, 90);
  el._destroy = () => clearInterval(el._interval);
  return el;
}

function showUserCard(msg) {
  const cat = userMsgCategory(msg);
  const card = document.createElement("div");
  card.className = `uq-card uq-card--${cat}`;
  card.dataset.msgId = msg.id;

  const fromLabel = msg.from || "agent";
  const subject = msg.subject || "";
  const content = msg.content || "";
  const msgProject = msg.project || "default";
  const needsReply = cat === "question";

  let html = `
    <div class="uq-header">
      <img class="uq-icon" src="${UQ_ICONS[cat] || UQ_ICONS.notification}" alt="${cat}" />
      <div class="uq-header-text">
        <span class="uq-from">${escapeHtml(fromLabel)}</span>
        <span class="uq-type">${userMsgTypeLabel(cat)}</span>
      </div>
    </div>
    <button class="uq-dismiss"><img src="/img/ui/icons/small/x.png" width="10" height="10" /></button>
    <span class="uq-project" data-project="${escapeHtml(msgProject)}">${escapeHtml(msgProject)}</span>
  `;

  if (subject) html += `<div class="uq-subject">${escapeHtml(subject)}</div>`;
  if (content) html += `<div class="uq-content">${escapeHtml(content)}</div>`;

  if (needsReply) {
    html += `
      <textarea placeholder="Type your response..."></textarea>
      <button class="uq-respond-btn">Respond</button>
    `;
  }

  card.innerHTML = html;

  // Navigate to project on click
  card.querySelector(".uq-project").addEventListener("click", () => {
    _navigateToProject(msgProject);
  });

  // Dismiss button
  card.querySelector(".uq-dismiss").addEventListener("click", () => {
    card.style.opacity = "0";
    card.style.transition = "opacity 0.3s ease";
    setTimeout(() => card.remove(), 300);
  });

  // Auto-dismiss notifications after 15s
  if (cat === "notification") {
    setTimeout(() => {
      if (card.parentNode) {
        card.style.opacity = "0";
        card.style.transition = "opacity 0.5s ease";
        setTimeout(() => card.remove(), 500);
      }
    }, 15000);
  }

  // Reply handling for questions
  if (needsReply) {
    const textarea = card.querySelector("textarea");
    const button = card.querySelector(".uq-respond-btn");

    button.addEventListener("click", async () => {
      const response = textarea.value.trim();
      if (!response) return;
      button.disabled = true;
      button.textContent = "";
      const loader = _uqLoadingWheel();
      button.appendChild(loader);

      const ok = await client.sendUserResponse(msgProject, msg.from, response, msg.id);
      loader._destroy();
      if (ok) {
        card.style.opacity = "0";
        card.style.transition = "opacity 0.3s ease";
        setTimeout(() => card.remove(), 300);
      } else {
        button.disabled = false;
        button.textContent = "Respond";
      }
    });
  }

  userQuestionsPanel.prepend(card);
}

function _navigateToProject(project, mode, taskId) {
  // If already in this colony, just switch mode
  if (viewMode === "colony" && colonyProject === project) {
    if (mode === "kanban") {
      setMode("kanban");
      if (taskId) setTimeout(() => kanbanBoard.highlightTask(taskId), 200);
    }
    return;
  }
  // Find the planet for this project and zoom in
  const planet = world.projectPlanets.find(p => p.project === project);
  if (planet) {
    zoomIntoColony(planet);
    if (mode === "kanban") {
      // Wait for colony to load, then switch to kanban
      setTimeout(() => {
        setMode("kanban");
        if (taskId) setTimeout(() => kanbanBoard.highlightTask(taskId), 300);
      }, 700);
    }
  } else {
    // No planet found, direct navigation
    setViewMode("colony", project);
    if (mode === "kanban") {
      setTimeout(() => {
        setMode("kanban");
        if (taskId) setTimeout(() => kanbanBoard.highlightTask(taskId), 300);
      }, 200);
    }
  }
}

function showUserTaskCard(task) {
  const card = document.createElement("div");
  const prio = task.priority || "P2";
  const isP0 = prio === "P0";
  card.className = `uq-card uq-card--task${isP0 ? " uq-card--task-p0" : ""}`;
  card.dataset.taskId = task.id;

  const from = task.dispatched_by || "agent";
  const prioIcon = isP0 ? "\u26A0" : prio === "P1" ? "\u25B2" : "\u25CF";
  const taskProject = task.project || "default";

  card.innerHTML = `
    <div class="uq-header">
      <img class="uq-icon" src="${UQ_ICONS.task}" alt="task" />
      <div class="uq-header-text">
        <span class="uq-from">${escapeHtml(from)} ${prioIcon}</span>
        <span class="uq-type">${escapeHtml(prio)} TASK</span>
      </div>
    </div>
    <button class="uq-dismiss"><img src="/img/ui/icons/small/x.png" width="10" height="10" /></button>
    <span class="uq-project" data-project="${escapeHtml(taskProject)}">${escapeHtml(taskProject)}</span>
    <div class="uq-subject">${escapeHtml(task.title || "(untitled)")}</div>
    ${task.description ? `<div class="uq-content uq-content--md">${renderMarkdown(task.description)}</div>` : ""}
    <div class="uq-actions">
      <button class="uq-respond-btn" data-action="accept">Accept</button>
      <button class="uq-respond-btn uq-respond-btn--done" data-action="done">Complete</button>
      <button class="uq-respond-btn uq-respond-btn--cancel" data-action="cancel">Cancel</button>
      <button class="uq-respond-btn uq-respond-btn--nav" data-action="kanban"><img src="/img/ui/icons/small/arrow_right.png" width="8" height="8" class="uq-btn-icon" />Kanban</button>
    </div>
  `;

  // Navigate to project
  card.querySelector(".uq-project").addEventListener("click", () => {
    _navigateToProject(taskProject);
  });

  // Navigate to kanban + highlight task
  card.querySelector('[data-action="kanban"]').addEventListener("click", () => {
    _navigateToProject(taskProject, "kanban", task.id);
  });

  card.querySelector(".uq-dismiss").addEventListener("click", () => {
    card.style.opacity = "0";
    card.style.transition = "opacity 0.3s ease";
    setTimeout(() => card.remove(), 300);
  });

  card.querySelector('[data-action="accept"]').addEventListener("click", async (e) => {
    const btn = e.target;
    btn.disabled = true;
    btn.textContent = "...";
    const result = await client.transitionTask(task.id, "in-progress", task.project || "default", "user");
    if (result) {
      btn.textContent = "Working";
      btn.style.background = "rgba(0,230,118,0.2)";
      btn.style.color = "#00e676";
    } else {
      btn.disabled = false;
      btn.textContent = "Accept";
    }
  });

  card.querySelector('[data-action="done"]').addEventListener("click", async (e) => {
    const btn = e.target;
    btn.disabled = true;
    btn.textContent = "...";
    const result = await client.transitionTask(task.id, "done", task.project || "default", "user");
    if (result) {
      card.style.opacity = "0";
      card.style.transition = "opacity 0.3s ease";
      setTimeout(() => card.remove(), 300);
    } else {
      btn.disabled = false;
      btn.textContent = "Done";
    }
  });

  card.querySelector('[data-action="cancel"]').addEventListener("click", async (e) => {
    const btn = e.target;
    btn.disabled = true;
    btn.textContent = "...";
    const result = await client.cancelTask(task.id, task.project || "default", "user");
    if (result) {
      card.style.opacity = "0";
      card.style.transition = "opacity 0.3s ease";
      setTimeout(() => card.remove(), 300);
    } else {
      btn.disabled = false;
      btn.textContent = "Cancel";
    }
  });

  userQuestionsPanel.prepend(card);
}

// --- Agent detail panel ---

// Section toggle logic
document.querySelectorAll(".dp-section-header[data-toggle]").forEach(header => {
  // Default: all sections open
  const bodyId = header.getAttribute("data-toggle");
  const body = document.getElementById(bodyId);
  if (body) { body.classList.add("open"); header.classList.add("open"); }
  header.addEventListener("click", () => {
    header.classList.toggle("open");
    if (body) body.classList.toggle("open");
  });
});

// Nav buttons
const dpNavPrev = document.getElementById("dp-nav-prev");
const dpNavNext = document.getElementById("dp-nav-next");
const dpNavLabel = document.getElementById("dp-nav-label");
if (dpNavPrev) dpNavPrev.addEventListener("click", () => {
  const keys = getAgentKeys();
  if (!keys.length) return;
  navIndex = (navIndex - 1 + keys.length) % keys.length;
  const av = agentViews.get(keys[navIndex]);
  if (av) { av.triggerRipple(); openDetail(av); }
});
if (dpNavNext) dpNavNext.addEventListener("click", () => {
  const keys = getAgentKeys();
  if (!keys.length) return;
  navIndex = (navIndex + 1) % keys.length;
  const av = agentViews.get(keys[navIndex]);
  if (av) { av.triggerRipple(); openDetail(av); }
});

const detailActivity = document.getElementById("detail-activity");
const detailStatusDot = document.getElementById("detail-status-dot");
const detailCurrentTask = document.getElementById("detail-current-task");

function openDetail(av) {
  // Clear previous selection ring
  for (const [, other] of agentViews) other.selected = false;
  av.selected = true;

  focusedAgent = agentKey(av.project, av.name);
  focusedTeam = null;
  // Side drawer no longer opens — command panel handles everything
  detailPanel.classList.remove("open");
  // Hide sidebar panels when agent focused — full width for canvas+command panel
  document.getElementById("main").classList.add("agent-focused");

  // ── MACRO: Identity ──
  detailName.textContent = av.name;
  detailName.style.color = av.color;
  const detailRoleEl = document.getElementById("detail-role");
  if (detailRoleEl) detailRoleEl.textContent = av.role || "";

  // Status dot + text
  if (detailStatusDot) {
    detailStatusDot.className = "dp-status-dot " + (av.sleeping ? "sleeping" : av.online ? "online" : "offline");
  }
  detailStatus.textContent = av.sleeping ? "Sleeping" : av.online ? "Online" : "Offline";
  detailStatus.style.color = av.sleeping ? "#9b59b6" : av.online ? "#00e676" : "#636e72";

  // Activity
  if (detailActivity) {
    if (av.activity && av.activity !== "idle") {
      const label = av.activityTool || av.activity;
      detailActivity.textContent = label;
      detailActivity.style.color = av.activity === "working" ? "#00e676" : "#a29bfe";
    } else {
      detailActivity.textContent = "";
    }
  }

  // ── MACRO: Current task (hero info) ──
  if (detailCurrentTask) {
    const agentTasks = allTasks.filter(t => {
      const tp = t.project || "default";
      return tp === av.project && (t.assigned_to === av.name || t.dispatched_by === av.name);
    }).filter(t => t.status !== "done");
    const current = agentTasks.find(t => t.status === "in-progress") || agentTasks[0];
    if (current) {
      const statusColors = { pending: "#ffd93d", accepted: "#74b9ff", "in-progress": "#00e676", blocked: "#ff6b6b" };
      const c = statusColors[current.status] || "#636e72";
      detailCurrentTask.innerHTML = `
        <div class="dp-current-task-label">Current Task</div>
        <div class="dp-current-task-title">${escapeHtml(current.title)}</div>
        <div class="dp-current-task-status" style="color:${c}">${current.status} · ${current.priority}</div>
      `;
    } else {
      detailCurrentTask.innerHTML = "";
    }
  }

  // ── MICRO: Details ──
  const detailDescEl = document.getElementById("detail-desc");
  if (detailDescEl) detailDescEl.textContent = av.description || "";
  const detailProjectEl = document.getElementById("detail-project");
  if (detailProjectEl) detailProjectEl.textContent = av.project !== "default" ? av.project : "";

  detailLastSeen.textContent = formatTime(av._lastSeenRaw);
  detailRegistered.textContent = formatTime(av._registeredRaw);

  // Reports To
  if (av._reportsTo) {
    detailReportsTo.innerHTML = "";
    const link = document.createElement("span");
    link.className = "detail-hierarchy-link";
    link.textContent = av._reportsTo;
    link.addEventListener("click", () => {
      const managerKey = agentKey(av.project, av._reportsTo);
      const managerAv = agentViews.get(managerKey);
      if (managerAv) openDetail(managerAv);
    });
    detailReportsTo.appendChild(link);
  } else {
    detailReportsTo.textContent = "\u2014";
  }

  // Direct Reports
  const directReports = [];
  for (const a of agentsData) {
    const aProject = a.project || "default";
    if (a.reports_to === av.name && aProject === av.project) {
      directReports.push(a.name);
    }
  }
  if (directReports.length > 0) {
    detailDirectReports.innerHTML = "";
    const container = document.createElement("div");
    container.className = "detail-reports-list";
    for (const name of directReports) {
      const tag = document.createElement("span");
      tag.className = "detail-report-tag";
      tag.textContent = name;
      tag.addEventListener("click", () => {
        const reportKey = agentKey(av.project, name);
        const reportAv = agentViews.get(reportKey);
        if (reportAv) openDetail(reportAv);
      });
      container.appendChild(tag);
    }
    detailDirectReports.appendChild(container);
  } else {
    detailDirectReports.textContent = "\u2014";
  }

  // Teams
  const detailTeamsEl = document.getElementById("detail-teams");
  if (detailTeamsEl) {
    const teams = av._teams || [];
    if (teams.length > 0) {
      detailTeamsEl.innerHTML = "";
      const container = document.createElement("div");
      container.className = "detail-reports-list";
      for (const t of teams) {
        const tag = document.createElement("span");
        tag.className = `detail-team-tag detail-team-type-${t.type}`;
        tag.textContent = t.type === "admin" ? `★ ${t.name}` : t.name;
        tag.title = `Role: ${t.role} | Type: ${t.type}`;
        container.appendChild(tag);
      }
      detailTeamsEl.appendChild(container);
    } else {
      detailTeamsEl.textContent = "\u2014";
    }
  }

  // All tasks
  const detailTasksEl = document.getElementById("detail-tasks");
  if (detailTasksEl) {
    const agentTasks = allTasks.filter(t => {
      const tp = t.project || "default";
      return tp === av.project && (t.assigned_to === av.name || t.dispatched_by === av.name);
    }).filter(t => t.status !== "done").slice(0, 8);
    if (agentTasks.length > 0) {
      detailTasksEl.innerHTML = agentTasks.map(t => {
        const statusColors = { pending: "#ffd93d", accepted: "#74b9ff", "in-progress": "#00e676", blocked: "#ff6b6b" };
        const color = statusColors[t.status] || "#636e72";
        return `<div class="detail-task-item">
          <span style="color:${color}">[${t.status}]</span>
          <span>${escapeHtml(t.title.length > 35 ? t.title.slice(0, 33) + "..." : t.title)}</span>
          <span style="color:#636e72;margin-left:auto">${t.priority}</span>
        </div>`;
      }).join("");
    } else {
      detailTasksEl.textContent = "\u2014";
    }
  }

  // Recent comms — last 3 messages involving this agent
  const detailRecentMsgs = document.getElementById("detail-recent-msgs");
  if (detailRecentMsgs) {
    client.fetchAllMessagesAllProjects().then(allMsgs => {
      const agentMsgs = allMsgs.filter(m => {
        const mp = m.project || "default";
        return mp === av.project && (m.from === av.name || m.to === av.name);
      }).slice(-5).reverse();

      if (agentMsgs.length > 0) {
        detailRecentMsgs.innerHTML = agentMsgs.map(m => {
          const isSent = m.from === av.name;
          const peer = isSent ? (m.to || "broadcast") : m.from;
          const dir = isSent ? "→" : "←";
          const dirColor = isSent ? "#a29bfe" : "#74b9ff";
          const preview = m.content.length > 60 ? m.content.slice(0, 58) + "..." : m.content;
          const conv = m.conversation_id ? conversations.find(c => c.id === m.conversation_id) : null;
          const convTag = conv ? `<span class="dp-msg-conv">${escapeHtml(conv.title || "conv")}</span>` : "";
          return `<div class="dp-msg-item">
            <span class="dp-msg-dir" style="color:${dirColor}">${dir}</span>
            <span class="dp-msg-peer">${escapeHtml(peer)}</span>
            ${convTag}
            <div class="dp-msg-preview">${escapeHtml(preview)}</div>
            <span class="dp-msg-time">${formatTime(m.created_at)}</span>
          </div>`;
        }).join("");
      } else {
        detailRecentMsgs.innerHTML = '<span style="color:#5a5e78">No messages</span>';
      }
    });
  }

  // Token usage
  updateAgentTokenDetail(av.project, av.name);

  // Nav label
  const keys = getAgentKeys();
  const currentIdx = keys.indexOf(focusedAgent);
  if (currentIdx >= 0) navIndex = currentIdx;
  if (dpNavLabel) dpNavLabel.textContent = `${navIndex + 1} / ${keys.length}`;

  // Dim other agents
  for (const [key, other] of agentViews) {
    const isFocused = key === focusedAgent;
    other.highlighted = isFocused;
    other.dimMode = !isFocused;
  }

  loadMessages();

  // Feed command panel with ALL agent data (replaces side drawer)
  (async () => {
    const slug = av.name;
    const [profile, allMsgs] = await Promise.all([
      client.fetchProfile(slug, av.project),
      client.fetchAllMessagesAllProjects(),
    ]);

    // Agent tasks
    const agentTasks = allTasks.filter(t => {
      const tp = t.project || "default";
      return tp === av.project && (t.assigned_to === av.name || t.dispatched_by === av.name);
    }).filter(t => t.status !== "done").slice(0, 10);

    // Recent messages
    const agentMsgs = allMsgs.filter(m => {
      const mp = m.project || "default";
      return mp === av.project && (m.from === av.name || m.to === av.name);
    }).slice(-8).reverse();

    // Direct reports
    const directReports = agentsData
      .filter(a => a.reports_to === av.name && (a.project || "default") === av.project)
      .map(a => a.name);

    // Nav label
    const keys = getAgentKeys();
    const idx = keys.indexOf(focusedAgent);

    const agentData = {
      ...av,
      slug: profile?.slug || slug,
      name: profile?.name || av.name,
      role: profile?.role || av.role,
      _tasks: agentTasks,
      _recentMsgs: agentMsgs,
      _reportsTo: av._reportsTo,
      _directReports: directReports,
      _teams: av._teams || [],
      _navLabel: `${idx + 1} / ${keys.length}`,
    };
    commandPanel.setAgent(agentData);
  })();
}

detailClose.addEventListener("click", () => {
  detailPanel.classList.remove("open");
  document.getElementById("main").classList.remove("agent-focused");
  focusedAgent = null;
  // Restore all agents + clear selection ring
  for (const [, av] of agentViews) {
    av.highlighted = true;
    av.dimMode = false;
    av.selected = false;
  }
  commandPanel.clearAgent();
  loadMessages();
});

// --- Pan/Zoom input handlers ---

let dragging = false;
let dragStartX = 0;
let dragStartY = 0;
let dragMoved = false;

canvas.addEventListener("mousedown", (e) => {
  const rect = canvas.getBoundingClientRect();
  const sx = e.clientX - rect.left;
  const sy = e.clientY - rect.top;

  // Check if clicking on an agent (don't start pan)
  const wp = engine.camera.screenToWorld(sx, sy, engine.width, engine.height);
  for (const [, av] of agentViews) {
    if (av.hitTest(wp.x, wp.y)) {
      return; // Let the click handler deal with it
    }
  }

  dragging = true;
  dragMoved = false;
  dragStartX = e.clientX;
  dragStartY = e.clientY;
});

canvas.addEventListener("mousemove", (e) => {
  if (dragging) {
    const dx = e.clientX - dragStartX;
    const dy = e.clientY - dragStartY;
    if (Math.abs(dx) > 2 || Math.abs(dy) > 2) {
      dragMoved = true;
    }
    engine.camera.pan(dx, dy);
    dragStartX = e.clientX;
    dragStartY = e.clientY;
    canvas.style.cursor = "grabbing";
    return;
  }

  // Hover cursor + agent hover state
  const rect = canvas.getBoundingClientRect();
  const sx = e.clientX - rect.left;
  const sy = e.clientY - rect.top;
  const wp = engine.camera.screenToWorld(sx, sy, engine.width, engine.height);

  let newHovered = null;
  for (const [key, av] of agentViews) {
    if (av.hitTest(wp.x, wp.y)) {
      newHovered = key;
      break;
    }
  }

  if (newHovered !== hoveredAgentKey) {
    if (hoveredAgentKey) {
      const prev = agentViews.get(hoveredAgentKey);
      if (prev) prev.hovered = false;
    }
    if (newHovered) {
      const next = agentViews.get(newHovered);
      if (next) next.hovered = true;
    }
    hoveredAgentKey = newHovered;
  }

  // Planet hover detection (galaxy view only)
  let newHoveredPlanet = null;
  if (viewMode === "galaxy" && world.projectPlanets.length > 0) {
    for (const planet of world.projectPlanets) {
      const hitR = planet.size * 0.6;
      const dx = wp.x - planet.cx;
      const dy = wp.y - planet.cy;
      if (dx * dx + dy * dy <= hitR * hitR) {
        newHoveredPlanet = planet.project;
        break;
      }
    }
  }
  world.hoveredPlanet = newHoveredPlanet;
  // Provide project stats for tooltip
  if (newHoveredPlanet && !world._projectStats) world._projectStats = {};
  if (newHoveredPlanet) {
    const p = newHoveredPlanet;
    // Prefer projectsData (from /api/projects with pre-computed stats)
    const projInfo = projectsData.find(pi => pi.name === p);
    if (projInfo) {
      world._projectStats[p] = {
        total: projInfo.agent_count, online: projInfo.online_count,
        tasks: projInfo.total_tasks, active: projInfo.active_tasks, done: projInfo.done_tasks,
      };
    } else {
      const agents = agentsData.filter(a => (a.project || "default") === p);
      const online = agents.filter(a => a.online || a.status === "busy").length;
      const tasks = allTasks.filter(t => (t.project || "default") === p);
      const active = tasks.filter(t => t.status === "in-progress").length;
      const done = tasks.filter(t => t.status === "done").length;
      world._projectStats[p] = { total: agents.length, online, tasks: tasks.length, active, done };
    }
  }

  canvas.style.cursor = (newHovered || newHoveredPlanet) ? "pointer" : "default";
});

canvas.addEventListener("mouseup", () => {
  dragging = false;
  canvas.style.cursor = "default";
});

canvas.addEventListener("mouseleave", () => {
  dragging = false;
  if (hoveredAgentKey) {
    const prev = agentViews.get(hoveredAgentKey);
    if (prev) prev.hovered = false;
    hoveredAgentKey = null;
  }
});

// Click handler (uses world coords)
canvas.addEventListener("click", (e) => {
  if (dragMoved) return; // Was a pan drag, not a click

  const rect = canvas.getBoundingClientRect();
  const sx = e.clientX - rect.left;
  const sy = e.clientY - rect.top;
  const wp = engine.camera.screenToWorld(sx, sy, engine.width, engine.height);

  // 1. Check agent hit
  for (const [, av] of agentViews) {
    if (av.hitTest(wp.x, wp.y)) {
      av.triggerRipple();
      openDetail(av);
      return;
    }
  }

  // 2. Check team group hit (colony mode: click near a team cluster)
  if (viewMode === "colony" && colonyProject) {
    const project = colonyProject;
    // Collect unique team slugs
    const seenTeams = new Set();
    for (const a of agentsData) {
      if ((a.project || "default") !== project || !a.teams) continue;
      for (const t of a.teams) {
        if (t.type === "admin" || seenTeams.has(t.slug)) continue;
        seenTeams.add(t.slug);
        const memberKeys = getTeamMemberKeys(project, t.slug);
        if (memberKeys.length < 2) continue;
        // Compute team center from member positions
        let tcx = 0, tcy = 0, count = 0;
        for (const mk of memberKeys) {
          const mav = agentViews.get(mk);
          if (mav) { tcx += mav.x; tcy += mav.y; count++; }
        }
        if (count === 0) continue;
        tcx /= count; tcy /= count;
        // Compute bounding radius
        let maxR = 0;
        for (const mk of memberKeys) {
          const mav = agentViews.get(mk);
          if (mav) {
            const d = Math.sqrt((mav.x - tcx) ** 2 + (mav.y - tcy) ** 2);
            if (d > maxR) maxR = d;
          }
        }
        const hitR = maxR + 60;
        const tdx = wp.x - tcx;
        const tdy = wp.y - tcy;
        if (tdx * tdx + tdy * tdy <= hitR * hitR) {
          const memberNames = memberKeys.map(k => k.split(":")[1]);
          const alreadyFocused = focusedTeam && focusedTeam.slug === t.slug && focusedTeam.project === project;
          if (!alreadyFocused) {
            focusedTeam = { project, slug: t.slug, members: memberNames };
            focusedAgent = null;
            focusedProject = project;
            detailPanel.classList.remove("open");
            loadMessages();
            if (activeTab === "tasks") renderTasks();
            if (currentMode === "kanban") refreshKanban();
            return;
          }
          return;
        }
      }
    }
  }

  // 3. Check project planet hit → zoom-in animation then enter colony view
  if (viewMode === "galaxy") {
    for (const planet of world.projectPlanets) {
      const hitR = planet.size * 0.6;
      const dx = wp.x - planet.cx;
      const dy = wp.y - planet.cy;
      if (dx * dx + dy * dy <= hitR * hitR) {
        zoomIntoColony(planet);
        return;
      }
    }
    return; // Galaxy: click on empty space does nothing
  }

  // 4. Colony: click on empty space → clear focus (stay in colony)
  detailPanel.classList.remove("open");
  document.getElementById("main").classList.remove("agent-focused");
  focusedAgent = null;
  focusedTeam = null;
  for (const [, av] of agentViews) {
    av.selected = false;
    av.highlighted = true;
    av.dimMode = false;
  }
  commandPanel.clearAgent();
  loadMessages();
});

// --- Asset picker (right-click sun / planet in galaxy view) ---
const assetPicker = document.getElementById("asset-picker");
const assetPickerTitle = document.getElementById("asset-picker-title");
const assetPickerGrid = document.getElementById("asset-picker-grid");
let currentDysonType = null; // null = cycle all, "1"-"7" = fixed, "off" = none

function closeAssetPicker() {
  assetPicker.classList.add("hidden");
}

function openDysonPicker() {
  assetPickerTitle.textContent = "DYSON SPHERE";
  assetPickerGrid.innerHTML = "";
  // "Cycle all" option
  const cycleItem = document.createElement("div");
  cycleItem.className = "picker-item" + (!currentDysonType ? " selected" : "");
  cycleItem.innerHTML = `<div style="width:100%;height:100%;display:flex;align-items:center;justify-content:center;font-size:8px;color:#ffd250;font-family:'JetBrains Mono',monospace">AUTO</div>`;
  cycleItem.addEventListener("click", async () => {
    currentDysonType = null;
    world._dysonType = null;
    await client.updateSettings({ dyson_type: "auto" });
    closeAssetPicker();
  });
  assetPickerGrid.appendChild(cycleItem);
  // "Off" option
  const offItem = document.createElement("div");
  offItem.className = "picker-item" + (currentDysonType === "off" ? " selected" : "");
  offItem.innerHTML = `<div style="width:100%;height:100%;display:flex;align-items:center;justify-content:center;font-size:8px;color:#888;font-family:'JetBrains Mono',monospace">OFF</div>`;
  offItem.addEventListener("click", async () => {
    currentDysonType = "off";
    world._dysonType = "off";
    await client.updateSettings({ dyson_type: "off" });
    closeAssetPicker();
  });
  assetPickerGrid.appendChild(offItem);
  // Individual frames
  for (let i = 1; i <= 7; i++) {
    const item = document.createElement("div");
    item.className = "picker-item" + (currentDysonType === String(i) ? " selected" : "");
    const img = new Image();
    img.src = `/img/space/dyson/${i}.png`;
    item.appendChild(img);
    item.addEventListener("click", async () => {
      currentDysonType = String(i);
      world._dysonType = currentDysonType;
      await client.updateSettings({ dyson_type: currentDysonType });
      closeAssetPicker();
    });
    assetPickerGrid.appendChild(item);
  }
  assetPicker.classList.remove("hidden");
}

// Load dyson_type from settings on init (deferred until client is ready)
function _loadDysonSettings() {
  if (typeof client === "undefined" || !client) return;
  client.fetchSettings().then(settings => {
    if (settings && settings.dyson_type && settings.dyson_type !== "auto") {
      currentDysonType = settings.dyson_type;
      world._dysonType = currentDysonType;
    }
    // Mode-awareness: linear → read-only board; native → writable .kb-form.
    if (settings && typeof settings.linear_mode === "boolean") {
      linearMode = settings.linear_mode;
      linearProject = (settings.linear && settings.linear.project) || "";
      kanbanBoard.setMode(isLinearBoard());
    }
  }).catch(() => {});
}

function openPlanetPicker(projectName) {
  const projInfo = projectsData.find(p => p.name === projectName);
  const currentType = projInfo ? projInfo.planet_type : "";
  assetPickerTitle.textContent = `PLANET: ${projectName.toUpperCase()}`;
  assetPickerGrid.innerHTML = "";
  for (const pt of spaceAssets.planetTypes) {
    const item = document.createElement("div");
    item.className = "picker-item" + (pt === currentType ? " selected" : "");
    const img = new Image();
    img.src = `/img/space/animated/${pt}/1.png`;
    item.appendChild(img);
    const label = document.createElement("div");
    label.className = "picker-label";
    label.textContent = pt.replace("/", " ");
    item.appendChild(label);
    item.addEventListener("click", async () => {
      const ok = await client.updateProjectPlanet(projectName, pt);
      if (ok) {
        // Refresh from DB to ensure persistence
        await fetchProjectsData();
        // Also update current world planet immediately
        const planet = world.projectPlanets.find(p => p.project === projectName);
        if (planet) planet.planetType = pt;
      }
      closeAssetPicker();
    });
    assetPickerGrid.appendChild(item);
  }
  assetPicker.classList.remove("hidden");
}

canvas.addEventListener("contextmenu", (e) => {
  if (viewMode !== "galaxy") return;
  e.preventDefault();
  const rect = canvas.getBoundingClientRect();
  const sx = e.clientX - rect.left;
  const sy = e.clientY - rect.top;
  const wp = engine.camera.screenToWorld(sx, sy, engine.width, engine.height);

  // Check sun hit (right-click to change Dyson sphere)
  if (world.sunCenter) {
    const dx = wp.x - world.sunCenter.cx;
    const dy = wp.y - world.sunCenter.cy;
    if (dx * dx + dy * dy <= 80 * 80) {
      openDysonPicker();
      return;
    }
  }

  // Check planet hit (right-click to change planet type)
  for (const planet of world.projectPlanets) {
    const hitR = planet.size * 0.6;
    const dx = wp.x - planet.cx;
    const dy = wp.y - planet.cy;
    if (dx * dx + dy * dy <= hitR * hitR) {
      openPlanetPicker(planet.project);
      return;
    }
  }
});

// Close picker on click outside or Escape
document.addEventListener("click", (e) => {
  if (!assetPicker.classList.contains("hidden") && !assetPicker.contains(e.target)) {
    closeAssetPicker();
  }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !assetPicker.classList.contains("hidden")) {
    closeAssetPicker();
    e.stopPropagation();
  }
});

// Zoom with wheel
canvas.addEventListener("wheel", (e) => {
  e.preventDefault();
  const rect = canvas.getBoundingClientRect();
  const sx = e.clientX - rect.left;
  const sy = e.clientY - rect.top;
  engine.camera.zoomAt(sx, sy, e.deltaY, engine.width, engine.height);
}, { passive: false });

// Re-layout + re-fit whenever the stage is actually re-rasterized
// (window resize or container box change via the engine's ResizeObserver).
engine.onResize = () => {
  layoutAgents();
  updateHierarchyLinks();
};

// --- Helpers ---

function formatTime(isoStr) {
  if (!isoStr) return "\u2014";
  try {
    const d = new Date(isoStr);
    return d.toLocaleTimeString("en", {
      hour12: false,
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
  } catch {
    return isoStr;
  }
}

function escapeHtml(str) {
  return String(str ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

/** Render markdown to HTML using marked (loaded via CDN). Falls back to escaped text. */
function renderMarkdown(text) {
  // Sanitize marked's HTML with DOMPurify before it ever reaches innerHTML —
  // task descriptions/messages are agent-controlled, so unsanitized markdown is
  // a stored-XSS sink. Require BOTH libs; if either is missing, fall back to
  // escaped text rather than inject raw HTML.
  if (typeof marked !== "undefined" && typeof DOMPurify !== "undefined") {
    try {
      return DOMPurify.sanitize(marked.parse(text, { gfm: true, breaks: true }));
    } catch (_) { /* fall through */ }
  }
  return escapeHtml(text).replace(/\n/g, "<br>");
}

// --- Memory panel ---

const tabMessages = document.getElementById("tab-messages");
const tabMemories = document.getElementById("tab-memories");
const messagesPanel = document.getElementById("messages-panel");
const memoriesPanel = document.getElementById("memories-panel");
const memoriesList = document.getElementById("memories-list");
const memoriesSearch = document.getElementById("memories-search");
const memoriesScopeFilter = document.getElementById("memories-scope-filter");
const memoriesProjectFilter = document.getElementById("memories-project-filter");
const memoryCountEl = document.getElementById("memory-count");

const tabTasks = document.getElementById("tab-tasks");
const tasksPanel = document.getElementById("tasks-panel");
const tasksList = document.getElementById("tasks-list");
const tasksStatusFilter = document.getElementById("tasks-status-filter");
const tasksPriorityFilter = document.getElementById("tasks-priority-filter");
const tasksMineFilter = document.getElementById("tasks-mine-filter");
const taskCountEl = document.getElementById("task-count");
let showMyTasksOnly = false;

let activeTab = "messages";

tabMessages.addEventListener("click", () => {
  activeTab = "messages";
  tabMessages.classList.add("active");
  tabMemories.classList.remove("active");
  tabTasks.classList.remove("active");
  messagesPanel.classList.remove("hidden");
  memoriesPanel.classList.add("hidden");
  tasksPanel.classList.add("hidden");
  if (currentMode === "kanban" || currentMode === "stats" || currentMode === "notifications") setMode("canvas");
});

tabMemories.addEventListener("click", () => {
  activeTab = "memories";
  tabMemories.classList.add("active");
  tabMessages.classList.remove("active");
  tabTasks.classList.remove("active");
  memoriesPanel.classList.remove("hidden");
  messagesPanel.classList.add("hidden");
  tasksPanel.classList.add("hidden");
  if (currentMode === "kanban" || currentMode === "stats" || currentMode === "notifications") setMode("canvas");
  loadMemories();
});

let searchTimeout = null;
memoriesSearch.addEventListener("input", () => {
  clearTimeout(searchTimeout);
  searchTimeout = setTimeout(() => loadMemories(), 300);
});
memoriesScopeFilter.addEventListener("change", () => loadMemories());
memoriesProjectFilter.addEventListener("change", () => loadMemories());

tabTasks.addEventListener("click", () => {
  activeTab = "tasks";
  tabTasks.classList.add("active");
  tabMessages.classList.remove("active");
  tabMemories.classList.remove("active");
  tasksPanel.classList.remove("hidden");
  messagesPanel.classList.add("hidden");
  memoriesPanel.classList.add("hidden");
  if (currentMode === "kanban" || currentMode === "stats" || currentMode === "notifications") setMode("canvas");
  loadTasks();
});

tasksStatusFilter.addEventListener("change", () => renderTasks());
tasksPriorityFilter.addEventListener("change", () => renderTasks());
tasksMineFilter.addEventListener("click", () => {
  showMyTasksOnly = !showMyTasksOnly;
  tasksMineFilter.classList.toggle("active", showMyTasksOnly);
  renderTasks();
});

async function loadMemories() {
  const query = memoriesSearch.value.trim();
  const scope = memoriesScopeFilter.value;
  const project = memoriesProjectFilter.value;

  let memories;
  if (query) {
    memories = await client.searchMemories(query);
    if (scope) memories = memories.filter(m => m.scope === scope);
    if (project) memories = memories.filter(m => m.project === project);
  } else {
    memories = await client.fetchMemories({ scope, project });
  }

  memoryCountEl.textContent = memories.length;
  renderMemories(memories);
}

function renderMemories(memories) {
  memoriesList.innerHTML = "";

  if (memories.length === 0) {
    memoriesList.innerHTML = '<div class="msg-empty">No memories yet</div>';
    return;
  }

  // Update project filter options from data
  const projects = new Set(memories.map(m => m.project));
  const currentVal = memoriesProjectFilter.value;
  memoriesProjectFilter.innerHTML = '<option value="">All projects</option>';
  for (const p of [...projects].sort()) {
    const opt = document.createElement("option");
    opt.value = p;
    opt.textContent = p;
    if (p === currentVal) opt.selected = true;
    memoriesProjectFilter.appendChild(opt);
  }

  for (const mem of memories) {
    const el = document.createElement("div");
    el.className = "memory-item" + (mem.conflict_with ? " memory-conflict" : "");

    const tags = parseTags(mem.tags);
    const tagsHtml = tags.map(t => `<span class="memory-tag">${escapeHtml(t)}</span>`).join("");

    const val = mem.value.length > 200 ? mem.value.slice(0, 200) + "..." : mem.value;
    const time = formatTime(mem.updated_at);

    el.innerHTML = `
      <div class="memory-header">
        <span class="memory-key">${escapeHtml(mem.key)}</span>
        <span class="memory-scope memory-scope-${mem.scope}">${mem.scope}</span>
        ${mem.conflict_with ? '<span class="memory-conflict-badge">CONFLICT</span>' : ""}
      </div>
      <div class="memory-value">${escapeHtml(val)}</div>
      <div class="memory-meta">
        <span class="memory-agent">${escapeHtml(mem.agent_name)}</span>
        <span class="memory-confidence">${mem.confidence}</span>
        <span class="memory-version">v${mem.version}</span>
        ${tagsHtml}
        <span class="memory-time">${time}</span>
      </div>
      <div class="memory-actions">
        <button class="memory-delete-btn" title="Archive">&#x2715;</button>
      </div>
    `;

    el.querySelector(".memory-delete-btn").addEventListener("click", async (e) => {
      e.stopPropagation();
      const ok = await client.deleteMemory(mem.id);
      if (ok) {
        el.style.opacity = "0";
        setTimeout(() => { el.remove(); loadMemories(); }, 200);
      }
    });

    el.addEventListener("click", () => {
      const existing = el.querySelector(".memory-expanded");
      if (existing) {
        existing.remove();
        return;
      }
      const expanded = document.createElement("div");
      expanded.className = "memory-expanded";
      expanded.textContent = mem.value;
      el.appendChild(expanded);
    });

    memoriesList.appendChild(el);
  }
}

function parseTags(tagsStr) {
  try {
    const parsed = JSON.parse(tagsStr);
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    return [];
  }
}

// Poll memories every 15s when tab is active
setInterval(() => {
  if (activeTab === "memories") loadMemories();
}, 15000);

// --- Tasks panel ---

let allTasks = [];

function onNewTasks(tasks) {
  for (const task of tasks) {
    const idx = allTasks.findIndex(t => t.id === task.id);
    if (idx >= 0) {
      allTasks[idx] = task;
    } else {
      allTasks.push(task);
    }
  }
  taskCountEl.textContent = allTasks.filter(t => t.status !== "done").length;
  if (activeTab === "tasks") renderTasks();
  updateAgentTaskLabels();

  // Canvas effects for new task completions (only fire once per task)
  for (const task of tasks) {
    if (task.status === "done" && task.assigned_to && !_celebratedTasks.has(task.id)) {
      _celebratedTasks.add(task.id);
      const key = agentKey(task.project || "default", task.assigned_to);
      const av = agentViews.get(key);
      if (av) av.particles.emit("celebrate", av.x, av.y - 10);
    }
  }

  // Notify user of tasks dispatched to them
  for (const task of tasks) {
    const isForUser = (task.profile_slug === "user" || task.profile_slug === "founder" || task.profile_slug === "human")
      && task.status === "pending" && !shownUserMsgs.has("task:" + task.id);
    if (isForUser) {
      shownUserMsgs.add("task:" + task.id);
      showUserTaskCard(task);
    }
  }

  // Update kanban if visible — re-fetch the cycle-scoped board (mirror) rather
  // than feeding unfiltered allTasks, so the cycle filter stays consistent.
  if (currentMode === "kanban") {
    refreshKanban();
  }
}

async function loadTasks() {
  const tasks = await client.fetchAllTasks();
  allTasks = tasks;
  taskCountEl.textContent = allTasks.filter(t => t.status !== "done").length;
  renderTasks();
}

/** Filter tasks by current view context (global > project > team > agent). */
function getViewFilteredTasks() {
  if (focusedAgent) {
    const av = agentViews.get(focusedAgent);
    if (!av) return [];
    return allTasks.filter(t => {
      const tp = t.project || "default";
      return tp === av.project && (t.assigned_to === av.name || t.dispatched_by === av.name || t.profile_slug === av.name);
    });
  } else if (focusedTeam) {
    const memberSet = new Set(focusedTeam.members);
    return allTasks.filter(t => {
      const tp = t.project || "default";
      return tp === focusedTeam.project && (memberSet.has(t.assigned_to) || memberSet.has(t.dispatched_by));
    });
  } else if (focusedProject) {
    return allTasks.filter(t => (t.project || "default") === focusedProject);
  }
  return allTasks;
}

function esc(s) {
  if (s == null) return "";
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function renderTasks() {
  const status = tasksStatusFilter.value;
  const priority = tasksPriorityFilter.value;

  let filtered = getViewFilteredTasks();
  if (showMyTasksOnly) filtered = filtered.filter(t => (t.profile_slug === "user" || t.profile_slug === "founder" || t.profile_slug === "human") && t.status !== "done");
  if (status) filtered = filtered.filter(t => t.status === status);
  if (priority) filtered = filtered.filter(t => t.priority === priority);

  if (filtered.length === 0) {
    tasksList.innerHTML = '<div class="msg-empty">No tasks</div>';
    return;
  }

  // Build set of task IDs we want to show
  const wantedIds = new Set(filtered.map(t => t.id));

  // Remove items no longer in the filtered list
  tasksList.querySelectorAll(".task-item[data-task-id]").forEach(el => {
    if (!wantedIds.has(el.dataset.taskId)) el.remove();
  });

  // Build map of existing DOM items
  const existingEls = new Map();
  tasksList.querySelectorAll(".task-item[data-task-id]").forEach(el => {
    existingEls.set(el.dataset.taskId, el);
  });

  // Remove empty placeholder if present
  const empty = tasksList.querySelector(".msg-empty");
  if (empty) empty.remove();

  for (const task of filtered) {
    const existing = existingEls.get(task.id);
    if (existing) {
      // Update in-place: status, priority, title
      const statusEl = existing.querySelector(".task-status");
      const prioEl = existing.querySelector(".task-priority");
      const titleEl = existing.querySelector(".task-title");
      if (statusEl && statusEl.textContent !== task.status) {
        statusEl.textContent = task.status;
        statusEl.className = `task-status task-status-${task.status}`;
      }
      if (prioEl && prioEl.textContent !== task.priority) {
        prioEl.textContent = task.priority;
        prioEl.className = `task-priority task-priority-${task.priority}`;
      }
      if (titleEl) titleEl.textContent = task.title;
      continue;
    }

    const el = document.createElement("div");
    const isMine = task.profile_slug === "user" || task.profile_slug === "founder" || task.profile_slug === "human";
    el.className = "task-item" + (isMine ? " task-mine" : "");
    el.dataset.taskId = task.id;

    const statusClass = `task-status-${task.status}`;
    const priorityClass = `task-priority-${task.priority}`;
    const time = formatTime(task.dispatched_at);
    const desc = task.description ? (task.description.length > 100 ? task.description.slice(0, 100) + "..." : task.description) : "";

    let extraHtml = "";
    if (task.status === "done" && task.result) {
      const r = task.result.length > 150 ? task.result.slice(0, 150) + "..." : task.result;
      extraHtml = `<div class="task-result">${escapeHtml(r)}</div>`;
    }
    if (task.status === "blocked" && task.blocked_reason) {
      extraHtml = `<div class="task-blocked-reason">${escapeHtml(task.blocked_reason)}</div>`;
    }

    el.innerHTML = `
      <div class="task-header">
        <span class="task-priority ${priorityClass}">${task.priority}</span>
        <span class="task-title">${escapeHtml(task.title)}</span>
        <span class="task-status ${statusClass}">${task.status}</span>
      </div>
      ${desc ? `<div class="task-description">${escapeHtml(desc)}</div>` : ""}
      ${extraHtml}
      <div class="task-meta">
        <span class="task-profile">${escapeHtml(task.profile_slug)}</span>
        ${task.assigned_to ? `<span class="task-agent">${escapeHtml(task.assigned_to)}</span>` : ""}
        <span class="task-time">${time}</span>
      </div>
    `;

    el.addEventListener("click", async () => {
      const existing = el.querySelector(".task-expanded");
      if (existing) { existing.remove(); return; }
      const expanded = document.createElement("div");
      expanded.className = "task-expanded";
      let details = `ID: ${task.id}\nProfile: ${task.profile_slug}\nDispatched by: ${task.dispatched_by}\nPriority: ${task.priority}\nStatus: ${task.status}`;
      if (task.assigned_to) details += `\nAssigned to: ${task.assigned_to}`;
      if (task.description) details += `\n\nDescription:\n${task.description}`;
      if (task.result) details += `\n\nResult:\n${task.result}`;
      if (task.blocked_reason) details += `\n\nBlocked: ${task.blocked_reason}`;
      expanded.textContent = details;
      el.appendChild(expanded);

      // Progress notes — surfaced between claim and complete for long-running tasks.
      try {
        const proj = task.project || "default";
        const resp = await fetch(`/api/tasks/${encodeURIComponent(task.id)}/progress?project=${encodeURIComponent(proj)}`);
        if (resp.ok) {
          const notes = await resp.json();
          if (Array.isArray(notes) && notes.length > 0) {
            const notesEl = document.createElement("div");
            notesEl.className = "task-progress-notes";
            const title = document.createElement("div");
            title.className = "task-progress-title";
            title.textContent = `Progress (${notes.length})`;
            notesEl.appendChild(title);
            for (const n of notes) {
              const line = document.createElement("div");
              line.className = "task-progress-note";
              line.textContent = `[${formatTime(n.created_at)}] ${n.agent}: ${n.note}`;
              notesEl.appendChild(line);
            }
            el.appendChild(notesEl);
          }
        }
      } catch (_e) {
        // best-effort — swallow fetch failures.
      }
    });

    tasksList.appendChild(el);
  }
}

// Poll tasks every 5s when tab is active
setInterval(() => {
  if (activeTab === "tasks") loadTasks();
}, 5000);

// Font scale is handled by the zoom controls block at the bottom of the file.

// --- Agent task label integration ---

function updateAgentTaskLabels() {
  // Clear all task labels first
  for (const [, av] of agentViews) {
    av.currentTaskLabel = null;
    av.isBlocked = false;
  }

  for (const task of allTasks) {
    if (!task.assigned_to) continue;
    const taskProject = task.project || "default";
    const key = agentKey(taskProject, task.assigned_to);
    const av = agentViews.get(key);
    if (!av) continue;

    if (task.status === "in-progress") {
      av.currentTaskLabel = task.title;
    } else if (task.status === "blocked") {
      av.currentTaskLabel = task.title;
      av.isBlocked = true;
    }
  }
}

// --- Connection overlay (teams + hierarchy) ---

function updateConnectionOverlay() {
  // Build teams with memberKeys from agentsData
  const teamMap = new Map(); // slug -> {slug, name, type, memberKeys: Set}

  for (const a of agentsData) {
    const project = a.project || "default";
    if (!a.teams) continue;
    for (const t of a.teams) {
      const teamKey = `${project}:${t.slug}`;
      if (!teamMap.has(teamKey)) {
        teamMap.set(teamKey, {
          slug: t.slug,
          name: t.name,
          type: t.type,
          memberKeys: [],
        });
      }
      teamMap.get(teamKey).memberKeys.push(agentKey(project, a.name));
    }
  }

  connectionOverlay.setData(agentViews, [...teamMap.values()]);
}

/** Get agent keys for all members of a team in a given project */
function getTeamMemberKeys(project, teamSlug) {
  const keys = [];
  for (const a of agentsData) {
    const aProject = a.project || "default";
    if (aProject !== project) continue;
    if (a.teams && a.teams.some(t => t.slug === teamSlug)) {
      keys.push(agentKey(project, a.name));
    }
  }
  return keys;
}

// Fetch teams periodically for the overlay
let _teamsFetchTimer = null;
async function fetchTeamsData() {
  teamsData = await client.fetchAllTeams();
}

// --- Message search & conversation filter ---

const msgSearchInput = document.getElementById("msg-search");
const msgConvFilter = document.getElementById("msg-conv-filter");

if (msgSearchInput) {
  let msgSearchTimeout = null;
  msgSearchInput.addEventListener("input", () => {
    clearTimeout(msgSearchTimeout);
    msgSearchTimeout = setTimeout(() => filterMessages(), 300);
  });
}
if (msgConvFilter) {
  msgConvFilter.addEventListener("change", () => {
    filterMessages();
    updateHighlights();
  });
}

function filterMessages() {
  const query = msgSearchInput ? msgSearchInput.value.trim().toLowerCase() : "";
  const convId = msgConvFilter ? msgConvFilter.value : "";
  const items = messagesList.querySelectorAll(".msg-item");
  for (const item of items) {
    const text = item.textContent.toLowerCase();
    const itemConv = item.dataset.convId || "";
    const matchSearch = !query || text.includes(query);
    const matchConv = !convId || itemConv === convId;
    item.style.display = (matchSearch && matchConv) ? "" : "none";
  }
}

// Update conversation filter options
function updateConvFilterOptions() {
  if (!msgConvFilter) return;
  const currentVal = msgConvFilter.value;
  msgConvFilter.innerHTML = '<option value="">All conversations</option>';
  for (const conv of conversations) {
    const opt = document.createElement("option");
    opt.value = conv.id;
    opt.textContent = conv.title || conv.id.slice(0, 8);
    if (conv.id === currentVal) opt.selected = true;
    msgConvFilter.appendChild(opt);
  }
}

// --- Layout modes ---

let currentMode = "canvas"; // "canvas" | "detail" | "kanban"
let viewMode = "galaxy"; // "galaxy" | "colony" — top-level screen state
let linearMode = false; // true when the relay mirrors Linear (legacy global flag)
let linearProject = ""; // the mirror project (e.g. "syn") — only ITS board is read-only

// Read-only applies to the Linear mirror project only; native projects keep
// their writable board even while the connector runs.
function isLinearBoard() {
  return !!linearProject && (focusedProject || "default") === linearProject;
}
let projectsData = []; // cached ProjectInfo[] from /api/projects
let colonyProject = null; // project name when in colony view

function setMode(mode) {
  currentMode = mode;
  const main = document.getElementById("main");
  main.classList.remove("mode-canvas", "mode-detail", "mode-kanban", "mode-stats", "mode-notifications");

  // Update header mode buttons
  document.querySelectorAll(".mode-btn").forEach(btn => {
    const isActive = btn.dataset.mode === mode;
    btn.classList.toggle("active", isActive);
    btn.setAttribute("aria-pressed", isActive ? "true" : "false");
  });

  // Show/hide kanban — fetch board (mirror read-replica) + cycles BEFORE showing.
  if (mode === "kanban") {
    kanbanBoard.setMode(isLinearBoard());
    const project = focusedProject || "default";
    Promise.all([
      client.fetchCycles(project),
      client.fetchBoardTasks(project, kanbanBoard.selectedCycle),
    ]).then(([cycles, tasks]) => {
      kanbanBoard.setCycles(cycles || []);
      kanbanBoard.setTasks(tasks || []);
      main.classList.add("mode-kanban");
      kanbanBoard.show();
    });
    statsPanel.hide();
  } else if (mode === "stats") {
    main.classList.add("mode-stats");
    kanbanBoard.hide();
    statsPanel.setProject(focusedProject || colonyProject || null);
    statsPanel.show();
  } else {
    main.classList.add(`mode-${mode}`);
    kanbanBoard.hide();
    statsPanel.hide();
  }

  // Show/hide notifications
  if (mode === "notifications") {
    main.classList.add("mode-notifications");
    notificationsView.show(focusedProject || "default");
  } else {
    notificationsView.hide();
  }

  // Messages panel: hidden in kanban/stats/notifications mode
  if (mode === "kanban" || mode === "stats" || mode === "notifications") {
    messagesPanel.classList.add("hidden");
    memoriesPanel.classList.add("hidden");
    tasksPanel.classList.add("hidden");
  } else if (activeTab === "messages") {
    messagesPanel.classList.remove("hidden");
  } else if (activeTab === "memories") {
    memoriesPanel.classList.remove("hidden");
  } else if (activeTab === "tasks") {
    tasksPanel.classList.remove("hidden");
  }
}

// Wire mode buttons
document.querySelectorAll(".mode-btn").forEach(btn => {
  btn.addEventListener("click", () => setMode(btn.dataset.mode));
});

// --- Galaxy / Colony view mode ---

const backGalaxyBtn = document.getElementById("back-galaxy");
const colonyProjectNameEl = document.getElementById("colony-project-name");
const colonyTokenValueEl = document.getElementById("colony-token-value");
const colonyTokenCallsEl = document.getElementById("colony-token-calls");
let _tokenPeriod = "24h";

function formatTokens(n) {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + "M";
  if (n >= 1000) return (n / 1000).toFixed(1) + "K";
  return String(n);
}

// Render an SVG sparkline into an <svg> element from time-series data
function renderSparkline(svgEl, buckets) {
  if (!svgEl || !buckets || buckets.length === 0) {
    if (svgEl) svgEl.innerHTML = "";
    return;
  }
  const vb = svgEl.viewBox.baseVal;
  const W = vb.width, H = vb.height;
  const pad = 2;
  const values = buckets.map(b => b.tokens);
  const max = Math.max(...values, 1);
  const n = values.length;

  const pts = values.map((v, i) => {
    const x = pad + (i / Math.max(n - 1, 1)) * (W - 2 * pad);
    const y = H - pad - (v / max) * (H - 2 * pad);
    return { x, y };
  });

  const lineD = pts.map((p, i) => (i === 0 ? "M" : "L") + p.x.toFixed(1) + "," + p.y.toFixed(1)).join(" ");
  const areaD = lineD + ` L${pts[pts.length - 1].x.toFixed(1)},${H} L${pts[0].x.toFixed(1)},${H} Z`;
  const last = pts[pts.length - 1];

  svgEl.innerHTML = `
    <path class="spark-area" d="${areaD}"/>
    <path class="spark-line" d="${lineD}"/>
    <circle class="spark-dot" cx="${last.x.toFixed(1)}" cy="${last.y.toFixed(1)}" r="2"/>
  `;
}

// Period pills
const pillsContainer = document.getElementById("token-period-pills");
if (pillsContainer) {
  pillsContainer.addEventListener("click", (e) => {
    const pill = e.target.closest(".token-pill");
    if (!pill) return;
    pillsContainer.querySelectorAll(".token-pill").forEach(p => p.classList.remove("active"));
    pill.classList.add("active");
    _tokenPeriod = pill.dataset.period;
    _tokenCurrent = { tokens: 0, calls: 0 };
    _tokenTarget = { tokens: 0, calls: 0 };
    if (colonyProject) startTokenPolling(colonyProject);
  });
}

let _zoomAnimating = false;

function zoomIntoColony(planet) {
  if (_zoomAnimating) return;
  _zoomAnimating = true;

  const canvas = document.getElementById("relay-canvas");
  const container = document.getElementById("canvas-container");
  if (!canvas || !container) {
    setViewMode("colony", planet.project);
    _zoomAnimating = false;
    return;
  }

  // Phase 1: Zoom into planet on galaxy canvas
  const originX = ((planet.cx / canvas.width) * 100).toFixed(1);
  const originY = ((planet.cy / canvas.height) * 100).toFixed(1);
  canvas.style.transformOrigin = `${originX}% ${originY}%`;
  canvas.style.transition = "transform 0.5s cubic-bezier(0.4, 0, 0.2, 1)";
  canvas.style.transform = "scale(3)";

  // Create flash overlay
  let flash = container.querySelector(".colony-flash");
  if (!flash) {
    flash = document.createElement("div");
    flash.className = "colony-flash";
    container.appendChild(flash);
  }

  // Phase 2: White flash at peak of zoom
  setTimeout(() => {
    flash.classList.add("active");
  }, 350);

  // Phase 3: Switch to colony behind the flash, then fade flash out
  setTimeout(() => {
    canvas.style.transition = "none";
    canvas.style.transform = "";
    canvas.style.transformOrigin = "";

    _teleportAgents = true;
    setViewMode("colony", planet.project);

    // Let colony render one frame, then fade flash out
    requestAnimationFrame(() => {
      flash.classList.remove("active");
      flash.classList.add("fading");

      setTimeout(() => {
        flash.classList.remove("fading");
        _zoomAnimating = false;
      }, 500);
    });
  }, 550);
}

function zoomOutToGalaxy() {
  if (_zoomAnimating) return;
  _zoomAnimating = true;

  const canvas = document.getElementById("relay-canvas");
  const container = document.getElementById("canvas-container");
  if (!canvas || !container) {
    setViewMode("galaxy");
    _zoomAnimating = false;
    return;
  }

  // Phase 1: Flash overlay
  let flash = container.querySelector(".colony-flash");
  if (!flash) {
    flash = document.createElement("div");
    flash.className = "colony-flash";
    container.appendChild(flash);
  }
  flash.classList.add("active");

  // Phase 2: Switch to galaxy behind flash, start zoomed in, then zoom out
  setTimeout(() => {
    // Pre-scale canvas before switching
    canvas.style.transition = "none";
    canvas.style.transform = "scale(3)";
    canvas.style.transformOrigin = "50% 50%";

    setViewMode("galaxy");

    requestAnimationFrame(() => {
      // Fade flash out while zooming out
      flash.classList.remove("active");
      flash.classList.add("fading");

      canvas.style.transition = "transform 0.5s cubic-bezier(0.4, 0, 0.2, 1)";
      canvas.style.transform = "scale(1)";

      setTimeout(() => {
        flash.classList.remove("fading");
        canvas.style.transition = "none";
        canvas.style.transform = "";
        canvas.style.transformOrigin = "";
        _zoomAnimating = false;
      }, 500);
    });
  }, 200);
}

function setViewMode(mode, project) {
  viewMode = mode;
  document.body.classList.remove("view-galaxy", "view-colony");
  document.body.classList.add(`view-${mode}`);

  if (mode === "galaxy") {
    colonyProject = null;
    focusedProject = null;
    focusedAgent = null;
    focusedTeam = null;
    detailPanel.classList.remove("open");
    document.getElementById("main").classList.remove("agent-focused");
    setMode("canvas");
    stopTokenPolling();
    startGalaxyTokenPolling();
    commandPanel.hide();
    // Defer layout to after DOM reflow so engine.width reflects new container size
    requestAnimationFrame(() => { engine.resize(); layoutAgents(); updateHierarchyLinks(); });
  } else if (mode === "colony" && project) {
    colonyProject = project;
    focusedProject = project;
    focusedAgent = null;
    focusedTeam = null;
    if (colonyProjectNameEl) colonyProjectNameEl.textContent = project.toUpperCase();
    setMode("canvas");
    commandPanel.show(project);
    requestAnimationFrame(() => { engine.resize(); layoutAgents(); updateHierarchyLinks(); });
    loadMessages();
    if (activeTab === "tasks") renderTasks();
    // Reset counters and start live polling
    stopGalaxyTokenPolling();
    _tokenCurrent = { tokens: 0, calls: 0 };
    _tokenTarget = { tokens: 0, calls: 0 };
    startTokenPolling(project);
  }
}

// Real-time token counter with counting animation
let _tokenCurrent = { tokens: 0, calls: 0 };
let _tokenTarget = { tokens: 0, calls: 0 };
let _tokenAnimFrame = null;
let _tokenPollTimer = null;

function animateTokenCounter() {
  let changed = false;
  // Animate tokens
  if (_tokenCurrent.tokens !== _tokenTarget.tokens) {
    const diff = _tokenTarget.tokens - _tokenCurrent.tokens;
    const step = Math.max(1, Math.abs(Math.ceil(diff / 12)));
    if (Math.abs(diff) <= step) {
      _tokenCurrent.tokens = _tokenTarget.tokens;
    } else {
      _tokenCurrent.tokens += diff > 0 ? step : -step;
    }
    changed = true;
  }
  // Animate calls
  if (_tokenCurrent.calls !== _tokenTarget.calls) {
    const diff = _tokenTarget.calls - _tokenCurrent.calls;
    const step = Math.max(1, Math.abs(Math.ceil(diff / 8)));
    if (Math.abs(diff) <= step) {
      _tokenCurrent.calls = _tokenTarget.calls;
    } else {
      _tokenCurrent.calls += diff > 0 ? step : -step;
    }
    changed = true;
  }
  // Update DOM
  if (colonyTokenValueEl) {
    colonyTokenValueEl.textContent = formatTokens(_tokenCurrent.tokens);
    colonyTokenValueEl.classList.toggle("ticking", changed);
  }
  if (colonyTokenCallsEl) {
    colonyTokenCallsEl.textContent = String(_tokenCurrent.calls);
    colonyTokenCallsEl.classList.toggle("ticking", changed);
  }
  // Continue animation if not done
  if (changed) {
    _tokenAnimFrame = requestAnimationFrame(animateTokenCounter);
  } else {
    _tokenAnimFrame = null;
  }
}

async function updateColonyTokenSummary(project) {
  if (!colonyTokenValueEl || !project) return;
  try {
    const data = await client.fetchTokenUsageByProject(project, _tokenPeriod);
    if (!Array.isArray(data)) return;
    _tokenTarget.tokens = data.reduce((sum, d) => sum + (d.tokens || 0), 0);
    _tokenTarget.calls = data.reduce((sum, d) => sum + (d.call_count || 0), 0);
    // Start counting animation if not already running
    if (!_tokenAnimFrame) {
      _tokenAnimFrame = requestAnimationFrame(animateTokenCounter);
    }
  } catch (err) {
    console.error("[token] colony summary error:", err);
  }
}

function startTokenPolling(project) {
  stopTokenPolling();
  updateColonyTokenSummary(project);
  _tokenPollTimer = setInterval(() => updateColonyTokenSummary(project), 5000);
}

function stopTokenPolling() {
  if (_tokenPollTimer) { clearInterval(_tokenPollTimer); _tokenPollTimer = null; }
  if (_tokenAnimFrame) { cancelAnimationFrame(_tokenAnimFrame); _tokenAnimFrame = null; }
}

// Galaxy-level token polling — updates planet badges in real-time
let _galaxyTokenTimer = null;

async function updateGalaxyTokens() {
  if (viewMode !== "galaxy" || typeof client === "undefined" || !client) return;
  try {
    const data = await client.fetchTokenUsage("24h");
    if (!Array.isArray(data) || !world._projectStats) return;
    for (const d of data) {
      if (world._projectStats[d.key]) {
        world._projectStats[d.key].tokens_24h = d.tokens || 0;
      }
    }
  } catch {}
}

function startGalaxyTokenPolling() {
  stopGalaxyTokenPolling();
  updateGalaxyTokens();
  _galaxyTokenTimer = setInterval(updateGalaxyTokens, 5000);
}

function stopGalaxyTokenPolling() {
  if (_galaxyTokenTimer) { clearInterval(_galaxyTokenTimer); _galaxyTokenTimer = null; }
}

// Populate token usage in agent detail panel
async function updateAgentTokenDetail(project, agentName) {
  const grid = document.getElementById("detail-token-grid");
  const toolsList = document.getElementById("detail-token-tools");
  const sparkSvg = document.getElementById("detail-sparkline");
  if (!grid || !toolsList) return;

  try {
    const [data24h, data7d, timeSeries, projectTotal] = await Promise.all([
      client.fetchTokenUsageByAgent(project, agentName, "24h"),
      client.fetchTokenUsageByAgent(project, agentName, "7d"),
      client.fetchTokenTimeSeries(project, "7d", agentName),
      client.fetchTokenUsageByProject(project, "24h"),
    ]);
    const sum = (arr) => ({ tokens: arr.reduce((s, d) => s + d.tokens, 0), calls: arr.reduce((s, d) => s + d.call_count, 0) });
    const s24 = sum(data24h);
    const s7d = sum(data7d);
    const projTotal24 = projectTotal.reduce((s, d) => s + d.tokens, 0);

    // Sparkline (7d activity)
    renderSparkline(sparkSvg, timeSeries);

    if (s24.tokens === 0 && s7d.tokens === 0) {
      grid.innerHTML = '<div style="color:rgba(224,224,232,0.3);font-size:9px;grid-column:1/-1;text-align:center;padding:8px">No token data yet</div>';
      toolsList.innerHTML = "";
      return;
    }

    // Derived stats
    const avg24 = s24.calls > 0 ? Math.round(s24.tokens / s24.calls) : 0;
    const pct = projTotal24 > 0 ? Math.round(s24.tokens / projTotal24 * 100) : 0;

    grid.innerHTML = `
      <div class="token-grid-cell tg-highlight"><span class="tg-value">${formatTokens(s24.tokens)}</span><span class="tg-label">tokens 24h</span></div>
      <div class="token-grid-cell"><span class="tg-value">${formatTokens(s7d.tokens)}</span><span class="tg-label">tokens 7d</span></div>
      <div class="token-grid-cell"><span class="tg-value">${s24.calls}</span><span class="tg-label">calls 24h</span></div>
      <div class="token-grid-cell"><span class="tg-value">${s7d.calls}</span><span class="tg-label">calls 7d</span></div>
      <div class="token-grid-cell"><span class="tg-value">${formatTokens(avg24)}</span><span class="tg-label">avg/call</span></div>
      <div class="token-grid-cell"><span class="tg-value">${pct}%</span><span class="tg-label">of project</span></div>
    `;

    // Tool breakdown (top 8)
    const maxTokens = data24h.length > 0 ? data24h[0].tokens : 1;
    toolsList.innerHTML = data24h.slice(0, 8).map(d => `
      <div class="token-tool-row">
        <span class="token-tool-name" title="${d.key}">${d.key}</span>
        <div class="token-tool-bar"><div class="token-tool-bar-fill" style="width:${Math.round(d.tokens / maxTokens * 100)}%"></div></div>
        <span class="token-tool-count">${formatTokens(d.tokens)}</span>
      </div>
    `).join("");
  } catch {
    grid.innerHTML = "";
    toolsList.innerHTML = "";
    if (sparkSvg) sparkSvg.innerHTML = "";
  }
}

if (backGalaxyBtn) {
  backGalaxyBtn.addEventListener("click", () => zoomOutToGalaxy());
}

async function fetchProjectsData() {
  const prev = projectsData.length;
  projectsData = await client.fetchProjects();
  // Re-layout if project list changed (new project appeared, etc.)
  if (projectsData.length !== prev && viewMode === "galaxy") {
    layoutAgents();
  }
}

async function fetchFileLockData() {
  const locks = await client.fetchFileLocks();
  // Group locks by agent+project
  const lockMap = new Map();
  for (const lock of locks) {
    const key = agentKey(lock.project || "default", lock.agent_name);
    if (!lockMap.has(key)) lockMap.set(key, []);
    lockMap.get(key).push(lock);
  }
  for (const [key, av] of agentViews) {
    av.fileLocks = lockMap.get(key) || [];
  }
}

// Initialize view mode
document.body.classList.add("view-galaxy");

// --- Typewriter effect for new messages ---

function typewriterAppend(el, text, speed = 12) {
  const contentEl = el.querySelector(".msg-content");
  if (!contentEl) return;
  contentEl.textContent = "";
  contentEl.classList.add("typing");
  let i = 0;
  const interval = setInterval(() => {
    if (i < text.length) {
      contentEl.textContent += text[i];
      i++;
    } else {
      clearInterval(interval);
      contentEl.classList.remove("typing");
    }
  }, speed);
}

// --- Kanban board ---

const kanbanPanel = document.getElementById("kanban-panel");
const kanbanBoard = new KanbanBoard(kanbanPanel);
window._kanbanBoard = kanbanBoard;
kanbanBoard.hide();

// --- Stats panel ---
const statsPanel = new StatsPanel(document.getElementById("stats-panel"));
window._statsPanel = statsPanel;
statsPanel.hide();

// Reload the board from the mirror read-replica (one call, cycle-aware).
async function refreshKanban() {
  const project = focusedProject || "default";
  const tasks = await client.fetchBoardTasks(project, kanbanBoard.selectedCycle);
  kanbanBoard.setTasks(tasks || []);
}

// Read-only card detail comments come from the relay's progress notes.
kanbanBoard.fetchProgress = (taskId, project) => client.fetchTaskProgress(taskId, project);

// Cycle filter change → re-fetch the board scoped to the chosen cycle.
kanbanBoard.onCycleChange = async () => {
  const project = focusedProject || "default";
  const [cycles, tasks] = await Promise.all([
    client.fetchCycles(project),
    client.fetchBoardTasks(project, kanbanBoard.selectedCycle),
  ]);
  kanbanBoard.setCycles(cycles || []);
  kanbanBoard.setTasks(tasks || []);
};

// Native-mode mutations (no-ops wired in linear/read-only mode by the board UI).
kanbanBoard.onTransition = async (taskId, newStatus, agentName) => {
  const project = focusedProject || "default";
  const result = await client.transitionTask(taskId, newStatus, project, agentName || "user");
  if (result) {
    kanbanBoard.upsertTask(result);
    updateAgentTaskLabels();
    if (activeTab === "tasks") renderTasks();
  }
};

kanbanBoard.onDispatch = async (data) => {
  const project = focusedProject || "default";
  const result = await client.dispatchTask({
    project,
    profile: data.profile,
    title: data.title,
    description: data.description,
    priority: data.priority,
    parent_task_id: data.parent_task_id || undefined,
  });
  if (result) {
    allTasks.push(result);
    kanbanBoard.upsertTask(result);
    taskCountEl.textContent = allTasks.filter(t => t.status !== "done").length;
  }
};

kanbanBoard.onDelete = async (taskId, project) => {
  const ok = await client.deleteTask(taskId, project);
  if (ok) {
    allTasks = allTasks.filter(t => t.id !== taskId);
    kanbanBoard.tasks = kanbanBoard.tasks.filter(t => t.id !== taskId);
    kanbanBoard.setTasks(kanbanBoard.tasks.slice());
    taskCountEl.textContent = allTasks.filter(t => t.status !== "done").length;
    if (activeTab === "tasks") renderTasks();
  }
};

kanbanBoard.onEdit = async (taskId, project, data) => {
  const result = await client.updateTask(taskId, { project, ...data });
  if (result) {
    const idx = allTasks.findIndex(t => t.id === taskId);
    if (idx >= 0) allTasks[idx] = result;
    kanbanBoard.upsertTask(result);
    if (activeTab === "tasks") renderTasks();
  }
};

// Real-time: SSE task lifecycle events upsert the board in place (no full
// reload). The event payload is minimal {agent, task_id, ...}; we re-fetch the
// single task to get the full mirror row, then upsert it. Wired in the start
// block once `client` exists.
function wireKanbanEvents() {
  client.subscribeTaskEvents(async (evt) => {
    const taskId = evt.semantic && evt.semantic.task_id;
    if (!taskId) return;
    // Only bother if the kanban is the active view (cheap, avoids noise).
    if (currentMode !== "kanban") return;
    const project = focusedProject || (evt.project || "default");
    const full = await client.fetchTask(taskId, project);
    if (full) kanbanBoard.upsertTask(full);
    else await refreshKanban(); // task may have been filtered/removed
  });
}

// --- Notifications panel ---
const notificationsPanel = document.getElementById("notifications-panel");
const notificationsView = new NotificationsPanel(notificationsPanel);
notificationsView.hide();
window._notificationsView = notificationsView;

// --- Keyboard shortcuts ---

const shortcuts = new ShortcutManager();

// Colony canvas sub-views
shortcuts.register("1", "mode-canvas", "Agents view", () => {
  if (viewMode === "colony") setMode("canvas");
});
shortcuts.register("2", "mode-kanban", "Kanban view", () => {
  if (viewMode === "colony") setMode("kanban");
});
shortcuts.register("3", "mode-stats", "Stats view", () => {
  if (viewMode === "colony") setMode("stats");
});
shortcuts.register("4", "mode-notifications", "Notifications view", () => {
  if (viewMode === "colony") setMode("notifications");
});

// Colony sidebar tabs
shortcuts.register("m", "tab-messages", "Messages tab", () => {
  if (viewMode !== "colony") return;
  tabMessages.click();
});
shortcuts.register("y", "tab-memories", "Memories tab", () => {
  if (viewMode !== "colony") return;
  tabMemories.click();
});
shortcuts.register("t", "tab-tasks", "Tasks tab", () => {
  if (viewMode !== "colony") return;
  tabTasks.click();
});

shortcuts.register("Escape", "close", "Close / return to galaxy", () => {
  const helpEl = document.getElementById("help-modal");
  if (helpEl && !helpEl.classList.contains("hidden")) {
    helpEl.classList.add("hidden");
  } else if (detailPanel.classList.contains("open")) {
    detailPanel.classList.remove("open");
    focusedAgent = null;
    loadMessages();
  } else if (viewMode === "colony" && currentMode !== "canvas") {
    setMode("canvas");
  } else if (viewMode === "colony") {
    zoomOutToGalaxy();
  }
});
shortcuts.register("g", "galaxy", "Back to galaxy", () => {
  if (viewMode === "colony") zoomOutToGalaxy();
});
shortcuts.register("ArrowUp", "colony-prev", "Previous colony", () => {
  if (viewMode !== "colony" || !projectsData.length) return;
  const names = projectsData.map(p => p.name);
  const idx = names.indexOf(colonyProject);
  const prev = (idx - 1 + names.length) % names.length;
  const planet = world.projectPlanets.find(p => p.project === names[prev]) || { project: names[prev] };
  setViewMode("colony", names[prev]);
});
shortcuts.register("ArrowDown", "colony-next", "Next colony", () => {
  if (viewMode !== "colony" || !projectsData.length) return;
  const names = projectsData.map(p => p.name);
  const idx = names.indexOf(colonyProject);
  const next = (idx + 1) % names.length;
  setViewMode("colony", names[next]);
});
shortcuts.register("/", "search", "Focus search", () => {
  if (viewMode !== "colony" || currentMode === "kanban") return;
  if (msgSearchInput) msgSearchInput.focus();
});
shortcuts.register("n", "new-task", "New task", () => {
  if (viewMode !== "colony") return;
  if (isLinearBoard()) return; // read-only on the mirror project — planning lives in Linear
  if (currentMode !== "kanban") setMode("kanban");
  kanbanBoard._showCreateForm();
});

// Agent navigation with arrows
let navIndex = -1;
function getAgentKeys() {
  const all = [...agentViews.keys()].sort();
  // In colony mode, only navigate agents from the current project
  if (viewMode === "colony" && colonyProject) {
    return all.filter(k => k.startsWith(colonyProject + ":"));
  }
  return all;
}

shortcuts.register("ArrowRight", "nav-next", "Next agent", () => {
  const keys = getAgentKeys();
  if (keys.length === 0) return;
  navIndex = (navIndex + 1) % keys.length;
  const av = agentViews.get(keys[navIndex]);
  if (av) { av.triggerRipple(); openDetail(av); }
});
shortcuts.register("ArrowLeft", "nav-prev", "Previous agent", () => {
  const keys = getAgentKeys();
  if (keys.length === 0) return;
  navIndex = (navIndex - 1 + keys.length) % keys.length;
  const av = agentViews.get(keys[navIndex]);
  if (av) { av.triggerRipple(); openDetail(av); }
});

// Font scale shortcuts are in the zoom controls block below.

shortcuts.register("?", "help", "Toggle help", () => toggleHelp());

shortcuts.start();

// --- Start ---

console.log("[relay] UI initializing...");
const client = new APIClient(onAgents, onConversations, onNewMessages, onNewTasks, onActivity);

// --- Command panel (wire client) ---
const commandPanel = new CommandPanel(
  document.getElementById('command-panel'),
  document.getElementById('cmd-resize-handle')
);
commandPanel.setClient(client);

// --- Command panel navigation ---
commandPanel.onNavigate = (arg) => {
  if (typeof arg === 'number') {
    // +1 or -1: prev/next agent
    const keys = getAgentKeys();
    if (!keys.length) return;
    navIndex = (navIndex + arg + keys.length) % keys.length;
    const av = agentViews.get(keys[navIndex]);
    if (av) { av.triggerRipple(); openDetail(av); }
  } else if (typeof arg === 'string') {
    // Agent name: navigate to specific agent (hierarchy click)
    const key = agentKey(colonyProject || 'default', arg);
    const av = agentViews.get(key);
    if (av) { av.triggerRipple(); openDetail(av); }
  }
};

_loadDysonSettings();
// Defer start to after first paint so canvas has correct dimensions
requestAnimationFrame(() => {
  engine.resize();
  client.start();
  wireKanbanEvents();
  loadMessages();
  loadTasks();
  fetchTeamsData();
  fetchProjectsData();
  _teamsFetchTimer = setInterval(fetchTeamsData, 10000);
  setInterval(fetchProjectsData, 10000);
  setInterval(fetchFileLockData, 5000);
  startGalaxyTokenPolling();
  console.log("[relay] polling started");
});

// --- Help modal ---
// --- Settings modal (Linear connector) ---
const settingsBtn = document.getElementById("settings-btn");
const settingsModal = document.getElementById("settings-modal");
const settingsClose = document.getElementById("settings-close");
if (settingsBtn && settingsModal) {
  const $ = (id) => document.getElementById(id);
  const openSettings = async () => {
    settingsModal.classList.remove("hidden");
    const st = await client.fetchSettings();
    const lin = (st && st.linear) || {};
    $("set-linear-enabled").checked = !!lin.enabled;
    $("set-linear-key").value = "";
    $("set-linear-key").placeholder = lin.api_key_masked ? `configurée (${lin.api_key_masked})` : "lin_api_...";
    $("set-linear-team-status").textContent = lin.team_key ? `team actuelle : ${lin.team_key}` : "";
    $("set-linear-status").textContent = lin.source === "env" ? "config par variables d'env (prioritaire)" : "";
  };
  settingsBtn.addEventListener("click", openSettings);
  settingsClose.addEventListener("click", () => settingsModal.classList.add("hidden"));
  settingsModal.querySelector(".help-overlay").addEventListener("click", () => settingsModal.classList.add("hidden"));

  $("set-linear-load-teams").addEventListener("click", async () => {
    const statusEl = $("set-linear-team-status");
    statusEl.textContent = "chargement…";
    const typed = $("set-linear-key").value.trim();
    const qs = typed ? `?key=${encodeURIComponent(typed)}` : "";
    try {
      const res = await fetch(`/api/linear/teams${qs}`);
      if (!res.ok) throw new Error((await res.json()).error || res.status);
      const teams = await res.json();
      const cur = (await client.fetchSettings())?.linear?.team_key || "";
      $("set-linear-teams").innerHTML = (teams || []).map(t => `
        <label><input type="radio" name="linear-team" value="${t.key}" ${t.key === cur ? "checked" : ""}>
          <span>${t.key} — ${t.name}</span>
          ${t.active_cycle ? `<span class="team-cycle">cycle actif : ${t.active_cycle}</span>` : ""}
        </label>`).join("");
      statusEl.textContent = `${teams.length} team(s)`;
    } catch (e) {
      statusEl.textContent = `erreur : ${e.message}`;
    }
  });

  $("set-linear-save").addEventListener("click", async () => {
    const statusEl = $("set-linear-status");
    const payload = { linear_enabled: $("set-linear-enabled").checked ? "1" : "0" };
    const typed = $("set-linear-key").value.trim();
    if (typed) payload.linear_api_key = typed;
    const team = settingsModal.querySelector('input[name="linear-team"]:checked');
    if (team) payload.linear_team_key = team.value;
    statusEl.textContent = "enregistrement…";
    await client.updateSettings(payload);
    const st = await client.fetchSettings();
    linearMode = !!(st && st.linear_mode);
    linearProject = (st && st.linear && st.linear.project) || "";
    statusEl.textContent = st?.linear?.enabled
      ? `actif — team ${st.linear.team_key}, poll ${st.linear.interval}`
      : "désactivé";
  });
}

const helpBtn = document.getElementById("help-btn");
const helpModal = document.getElementById("help-modal");
const helpClose = document.getElementById("help-close");
const helpOverlay = helpModal ? helpModal.querySelector(".help-overlay") : null;

function toggleHelp() {
  if (helpModal) helpModal.classList.toggle("hidden");
}
if (helpBtn) helpBtn.addEventListener("click", toggleHelp);
if (helpClose) helpClose.addEventListener("click", toggleHelp);
if (helpOverlay) helpOverlay.addEventListener("click", toggleHelp);

// --- Zoom controls ---
const ZOOM_STEPS = [0.8, 0.9, 1.0, 1.1, 1.2, 1.4, 1.6, 1.8, 2.0];
const ZOOM_DEFAULT = 2; // index of 1.0 in ZOOM_STEPS
let zoomIndex = parseInt(localStorage.getItem("relay-zoom") ?? ZOOM_DEFAULT, 10);
if (zoomIndex < 0 || zoomIndex >= ZOOM_STEPS.length) zoomIndex = ZOOM_DEFAULT;

const zoomInBtn = document.getElementById("zoom-in");
const zoomOutBtn = document.getElementById("zoom-out");
const zoomLabel = document.getElementById("zoom-level");

function applyZoom() {
  const scale = ZOOM_STEPS[zoomIndex];
  document.body.style.setProperty("--scale", scale);
  if (zoomLabel) zoomLabel.textContent = Math.round(scale * 100) + "%";
  localStorage.setItem("relay-zoom", zoomIndex);
}
applyZoom();

if (zoomInBtn) zoomInBtn.addEventListener("click", () => {
  if (zoomIndex < ZOOM_STEPS.length - 1) { zoomIndex++; applyZoom(); }
});
if (zoomOutBtn) zoomOutBtn.addEventListener("click", () => {
  if (zoomIndex > 0) { zoomIndex--; applyZoom(); }
});

shortcuts.register("+", "zoom-in", "Zoom in", () => {
  if (zoomIndex < ZOOM_STEPS.length - 1) { zoomIndex++; applyZoom(); }
});
shortcuts.register("-", "zoom-out", "Zoom out", () => {
  if (zoomIndex > 0) { zoomIndex--; applyZoom(); }
});
