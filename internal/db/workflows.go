package db

import (
	"time"

	"github.com/google/uuid"
)

// Workflow represents a visual DAG workflow definition.
type Workflow struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Nodes       string `json:"nodes"` // JSON array of node definitions
	Edges       string `json:"edges"` // JSON array of edge definitions
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// WorkflowRun represents a single execution of a workflow.
type WorkflowRun struct {
	ID           string `json:"id"`
	WorkflowID   string `json:"workflow_id"`
	Project      string `json:"project"`
	TriggerEvent string `json:"trigger_event,omitempty"`
	TriggerMeta  string `json:"trigger_meta,omitempty"`
	Status       string `json:"status"` // running, completed, failed, partial
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at,omitempty"`
	Error        string `json:"error,omitempty"`
}

// WorkflowNodeRun represents per-node execution within a workflow run.
type WorkflowNodeRun struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	NodeID     string `json:"node_id"`
	NodeType   string `json:"node_type"`
	Status     string `json:"status"` // pending, running, completed, failed, skipped
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

// CreateWorkflow inserts a new workflow definition.
func (d *DB) CreateWorkflow(project, name, description, nodesJSON, edgesJSON string) (*Workflow, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	id := uuid.New().String()
	if nodesJSON == "" {
		nodesJSON = "[]"
	}
	if edgesJSON == "" {
		edgesJSON = "[]"
	}

	_, err := d.conn.Exec(`
		INSERT INTO workflows (id, project, name, description, nodes, edges, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		id, project, name, description, nodesJSON, edgesJSON, now, now)
	if err != nil {
		return nil, err
	}

	return &Workflow{
		ID:          id,
		Project:     project,
		Name:        name,
		Description: description,
		Nodes:       nodesJSON,
		Edges:       edgesJSON,
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// UpdateWorkflow updates a workflow definition by ID.
func (d *DB) UpdateWorkflow(id, name, description, nodesJSON, edgesJSON string) (*Workflow, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)

	_, err := d.conn.Exec(`
		UPDATE workflows SET name = ?, description = ?, nodes = ?, edges = ?, updated_at = ?
		WHERE id = ?`,
		name, description, nodesJSON, edgesJSON, now, id)
	if err != nil {
		return nil, err
	}

	return d.GetWorkflow(id)
}

// GetWorkflow returns a single workflow by ID.
func (d *DB) GetWorkflow(id string) (*Workflow, error) {
	row := d.ro().QueryRow(`SELECT id, project, name, COALESCE(description, ''), nodes, edges, enabled, created_at, updated_at
		FROM workflows WHERE id = ?`, id)

	var w Workflow
	var enabled int
	if err := row.Scan(&w.ID, &w.Project, &w.Name, &w.Description, &w.Nodes, &w.Edges, &enabled, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, err
	}
	w.Enabled = enabled == 1
	return &w, nil
}

// ListWorkflows returns all workflows for a project.
func (d *DB) ListWorkflows(project string) ([]Workflow, error) {
	rows, err := d.ro().Query(`SELECT id, project, name, COALESCE(description, ''), nodes, edges, enabled, created_at, updated_at
		FROM workflows WHERE project = ? ORDER BY created_at LIMIT 200`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Workflow
	for rows.Next() {
		var w Workflow
		var enabled int
		if err := rows.Scan(&w.ID, &w.Project, &w.Name, &w.Description, &w.Nodes, &w.Edges, &enabled, &w.CreatedAt, &w.UpdatedAt); err != nil {
			continue
		}
		w.Enabled = enabled == 1
		result = append(result, w)
	}
	return result, nil
}

// DeleteWorkflow removes a workflow and cascades to runs and node runs.
func (d *DB) DeleteWorkflow(id string) error {
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete node runs for all runs of this workflow
	_, err = tx.Exec(`DELETE FROM workflow_node_runs WHERE run_id IN (
		SELECT id FROM workflow_runs WHERE workflow_id = ?)`, id)
	if err != nil {
		return err
	}

	// Delete runs
	_, err = tx.Exec(`DELETE FROM workflow_runs WHERE workflow_id = ?`, id)
	if err != nil {
		return err
	}

	// Delete workflow
	_, err = tx.Exec(`DELETE FROM workflows WHERE id = ?`, id)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ToggleWorkflow enables or disables a workflow.
func (d *DB) ToggleWorkflow(id string, enabled bool) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	e := 0
	if enabled {
		e = 1
	}
	_, _ = d.conn.Exec("UPDATE workflows SET enabled = ?, updated_at = ? WHERE id = ?", e, now, id)
}

// CreateWorkflowRun inserts a new workflow run.
func (d *DB) CreateWorkflowRun(workflowID, project, triggerEvent, triggerMeta string) (*WorkflowRun, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	id := uuid.New().String()
	if triggerMeta == "" {
		triggerMeta = "{}"
	}

	_, err := d.conn.Exec(`
		INSERT INTO workflow_runs (id, workflow_id, project, trigger_event, trigger_meta, status, started_at)
		VALUES (?, ?, ?, ?, ?, 'running', ?)`,
		id, workflowID, project, triggerEvent, triggerMeta, now)
	if err != nil {
		return nil, err
	}

	return &WorkflowRun{
		ID:           id,
		WorkflowID:   workflowID,
		Project:      project,
		TriggerEvent: triggerEvent,
		TriggerMeta:  triggerMeta,
		Status:       "running",
		StartedAt:    now,
	}, nil
}

// FinishWorkflowRun marks a workflow run as finished with status and optional error.
func (d *DB) FinishWorkflowRun(id, status, errMsg string) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, _ = d.conn.Exec(`UPDATE workflow_runs SET status = ?, finished_at = ?, error = ? WHERE id = ?`,
		status, now, errMsg, id)
}

// ListWorkflowRuns returns recent runs for a workflow.
func (d *DB) ListWorkflowRuns(workflowID string, limit int) ([]WorkflowRun, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := d.ro().Query(`SELECT id, workflow_id, project, COALESCE(trigger_event, ''), COALESCE(trigger_meta, '{}'),
		status, started_at, COALESCE(finished_at, ''), COALESCE(error, '')
		FROM workflow_runs WHERE workflow_id = ? ORDER BY started_at DESC LIMIT ?`, workflowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []WorkflowRun
	for rows.Next() {
		var r WorkflowRun
		if err := rows.Scan(&r.ID, &r.WorkflowID, &r.Project, &r.TriggerEvent, &r.TriggerMeta, &r.Status, &r.StartedAt, &r.FinishedAt, &r.Error); err != nil {
			continue
		}
		result = append(result, r)
	}
	return result, nil
}

// GetWorkflowRun returns a single workflow run by ID.
func (d *DB) GetWorkflowRun(id string) (*WorkflowRun, error) {
	row := d.ro().QueryRow(`SELECT id, workflow_id, project, COALESCE(trigger_event, ''), COALESCE(trigger_meta, '{}'),
		status, started_at, COALESCE(finished_at, ''), COALESCE(error, '')
		FROM workflow_runs WHERE id = ?`, id)

	var r WorkflowRun
	if err := row.Scan(&r.ID, &r.WorkflowID, &r.Project, &r.TriggerEvent, &r.TriggerMeta, &r.Status, &r.StartedAt, &r.FinishedAt, &r.Error); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateNodeRun inserts a new workflow node run with status=pending.
func (d *DB) CreateNodeRun(runID, nodeID, nodeType string) (*WorkflowNodeRun, error) {
	id := uuid.New().String()

	_, err := d.conn.Exec(`
		INSERT INTO workflow_node_runs (id, run_id, node_id, node_type, status, input, output)
		VALUES (?, ?, ?, ?, 'pending', '{}', '{}')`,
		id, runID, nodeID, nodeType)
	if err != nil {
		return nil, err
	}

	return &WorkflowNodeRun{
		ID:       id,
		RunID:    runID,
		NodeID:   nodeID,
		NodeType: nodeType,
		Status:   "pending",
		Input:    "{}",
		Output:   "{}",
	}, nil
}

// UpdateNodeRun updates a node run's status, output, error, and timestamps.
func (d *DB) UpdateNodeRun(id, status, outputJSON, errMsg string) {
	now := time.Now().UTC().Format(memoryTimeFmt)

	switch status {
	case "running":
		_, _ = d.conn.Exec(`UPDATE workflow_node_runs SET status = ?, started_at = ? WHERE id = ?`,
			status, now, id)
	case "completed", "failed", "skipped":
		_, _ = d.conn.Exec(`UPDATE workflow_node_runs SET status = ?, output = ?, error = ?, finished_at = ? WHERE id = ?`,
			status, outputJSON, errMsg, now, id)
	default:
		_, _ = d.conn.Exec(`UPDATE workflow_node_runs SET status = ?, output = ?, error = ? WHERE id = ?`,
			status, outputJSON, errMsg, id)
	}
}

// ListNodeRuns returns all node runs for a workflow run.
func (d *DB) ListNodeRuns(runID string) ([]WorkflowNodeRun, error) {
	rows, err := d.ro().Query(`SELECT id, run_id, node_id, node_type, status,
		COALESCE(input, '{}'), COALESCE(output, '{}'),
		COALESCE(started_at, ''), COALESCE(finished_at, ''), COALESCE(error, '')
		FROM workflow_node_runs WHERE run_id = ? ORDER BY started_at NULLS LAST`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []WorkflowNodeRun
	for rows.Next() {
		var n WorkflowNodeRun
		if err := rows.Scan(&n.ID, &n.RunID, &n.NodeID, &n.NodeType, &n.Status, &n.Input, &n.Output, &n.StartedAt, &n.FinishedAt, &n.Error); err != nil {
			continue
		}
		result = append(result, n)
	}
	return result, nil
}
