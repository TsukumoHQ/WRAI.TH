package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var validGoalTypes = map[string]bool{
	"mission":      true,
	"project_goal": true,
	"agent_goal":   true,
}

func scanGoal(row interface{ Scan(...any) error }) (models.Goal, error) {
	var g models.Goal
	err := row.Scan(&g.ID, &g.Project, &g.Type, &g.Title, &g.Description,
		&g.OwnerAgent, &g.ParentGoalID, &g.Status, &g.CreatedBy,
		&g.CreatedAt, &g.UpdatedAt, &g.CompletedAt)
	return g, err
}

const goalColumns = "id, project, type, title, description, owner_agent, parent_goal_id, status, created_by, created_at, updated_at, completed_at"

func (d *DB) CreateGoal(project, goalType, title, description, createdBy string, ownerAgent, parentGoalID *string) (*models.Goal, error) {
	if !validGoalTypes[goalType] {
		return nil, fmt.Errorf("invalid goal type: %s (must be mission, project_goal, or agent_goal)", goalType)
	}
	now := time.Now().UTC().Format(memoryTimeFmt)
	g := &models.Goal{
		ID:           uuid.New().String(),
		Project:      project,
		Type:         goalType,
		Title:        title,
		Description:  description,
		OwnerAgent:   ownerAgent,
		ParentGoalID: parentGoalID,
		Status:       "active",
		CreatedBy:    createdBy,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	_, err := d.conn.Exec(
		`INSERT INTO goals (id, project, type, title, description, owner_agent, parent_goal_id, status, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.Project, g.Type, g.Title, g.Description, g.OwnerAgent, g.ParentGoalID, g.Status, g.CreatedBy, g.CreatedAt, g.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create goal: %w", err)
	}
	return g, nil
}

func (d *DB) GetGoal(goalID, project string) (*models.Goal, error) {
	g, err := scanGoal(d.ro().QueryRow(
		"SELECT "+goalColumns+" FROM goals WHERE id = ? AND project = ?",
		goalID, project,
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get goal: %w", err)
	}
	return &g, nil
}

func (d *DB) ListGoals(project, goalType, status string, ownerAgent *string, limit int) ([]models.Goal, error) {
	if limit <= 0 {
		limit = 50
	}
	query := "SELECT " + goalColumns + " FROM goals WHERE project = ?"
	args := []any{project}

	if goalType != "" {
		query += " AND type = ?"
		args = append(args, goalType)
	}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	if ownerAgent != nil && *ownerAgent != "" {
		query += " AND owner_agent = ?"
		args = append(args, *ownerAgent)
	}

	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	return d.queryGoals(query, args...)
}

func (d *DB) ListAllGoals(limit int) ([]models.Goal, error) {
	if limit <= 0 {
		limit = 100
	}
	return d.queryGoals(
		"SELECT "+goalColumns+" FROM goals ORDER BY project, created_at DESC LIMIT ?",
		limit,
	)
}

func (d *DB) queryGoals(query string, args ...any) ([]models.Goal, error) {
	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var goals []models.Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		goals = append(goals, g)
	}
	return goals, rows.Err()
}

func (d *DB) UpdateGoal(goalID, project string, title, description, status *string) (*models.Goal, error) {
	goal, err := d.GetGoal(goalID, project)
	if err != nil {
		return nil, err
	}
	if goal == nil {
		return nil, fmt.Errorf("goal not found: %s", goalID)
	}

	now := time.Now().UTC().Format(memoryTimeFmt)
	if title != nil {
		goal.Title = *title
	}
	if description != nil {
		goal.Description = *description
	}
	if status != nil {
		goal.Status = *status
		if *status == "completed" {
			goal.CompletedAt = &now
		}
	}
	goal.UpdatedAt = now

	_, err = d.conn.Exec(
		"UPDATE goals SET title = ?, description = ?, status = ?, completed_at = ?, updated_at = ? WHERE id = ? AND project = ?",
		goal.Title, goal.Description, goal.Status, goal.CompletedAt, goal.UpdatedAt, goalID, project,
	)
	if err != nil {
		return nil, fmt.Errorf("update goal: %w", err)
	}
	return goal, nil
}

func (d *DB) GetGoalAncestry(goalID, project string) ([]models.Goal, error) {
	var chain []models.Goal
	currentID := goalID
	for i := 0; i < 5; i++ {
		var parentID *string
		err := d.ro().QueryRow("SELECT parent_goal_id FROM goals WHERE id = ? AND project = ?", currentID, project).Scan(&parentID)
		if err != nil || parentID == nil {
			break
		}
		parent, err := d.GetGoal(*parentID, project)
		if err != nil || parent == nil {
			break
		}
		chain = append(chain, *parent)
		currentID = *parentID
	}
	// Reverse to root-first order
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// goalProgress holds aggregated task counts for a single goal.
type goalProgress struct {
	total int
	done  int
}

// goalProgressByProject returns task counts for every goal in a project via a
// single GROUP BY query. Replaces the N+1 pattern of calling GetGoalProgress
// once per goal in GetGoalCascade. Goals with no tasks are simply absent from
// the map (callers read the zero value).
func (d *DB) goalProgressByProject(project string) map[string]goalProgress {
	out := map[string]goalProgress{}
	rows, err := d.ro().Query(
		`SELECT goal_id,
		        COUNT(*),
		        SUM(CASE WHEN status IN ('done','cancelled') THEN 1 ELSE 0 END)
		 FROM tasks
		 WHERE project = ? AND goal_id IS NOT NULL
		 GROUP BY goal_id`,
		project,
	)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var total, done int
		if err := rows.Scan(&id, &total, &done); err != nil {
			continue
		}
		out[id] = goalProgress{total: total, done: done}
	}
	return out
}

func (d *DB) GetGoalProgress(goalID, project string) (total int, done int) {
	_ = d.ro().QueryRow(
		"SELECT COUNT(*) FROM tasks WHERE goal_id = ? AND project = ?",
		goalID, project,
	).Scan(&total)
	_ = d.ro().QueryRow(
		"SELECT COUNT(*) FROM tasks WHERE goal_id = ? AND project = ? AND status IN ('done','cancelled')",
		goalID, project,
	).Scan(&done)
	return
}

func (d *DB) GetGoalChildren(goalID, project string) ([]models.Goal, error) {
	return d.queryGoals(
		"SELECT "+goalColumns+" FROM goals WHERE parent_goal_id = ? AND project = ? ORDER BY created_at",
		goalID, project,
	)
}

func (d *DB) GetGoalWithProgress(goalID, project string) (*models.GoalWithProgress, error) {
	goal, err := d.GetGoal(goalID, project)
	if err != nil || goal == nil {
		return nil, err
	}

	total, done := d.GetGoalProgress(goalID, project)
	ancestry, _ := d.GetGoalAncestry(goalID, project)
	children, _ := d.GetGoalChildren(goalID, project)

	var progress float64
	if total > 0 {
		progress = float64(done) / float64(total)
	}

	childrenWithProgress := make([]models.GoalWithProgress, 0, len(children))
	for _, c := range children {
		ct, cd := d.GetGoalProgress(c.ID, project)
		var cp float64
		if ct > 0 {
			cp = float64(cd) / float64(ct)
		}
		childrenWithProgress = append(childrenWithProgress, models.GoalWithProgress{
			Goal:       c,
			TotalTasks: ct,
			DoneTasks:  cd,
			Progress:   cp,
		})
	}

	if ancestry == nil {
		ancestry = []models.Goal{}
	}

	return &models.GoalWithProgress{
		Goal:       *goal,
		TotalTasks: total,
		DoneTasks:  done,
		Progress:   progress,
		Children:   childrenWithProgress,
		Ancestry:   ancestry,
	}, nil
}

func (d *DB) GetGoalCascade(project string) ([]models.GoalWithProgress, error) {
	goals, err := d.queryGoals(
		"SELECT "+goalColumns+" FROM goals WHERE project = ? ORDER BY created_at",
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("get goal cascade: %w", err)
	}

	// Fetch all per-goal task counts in ONE aggregate query instead of 2
	// queries per goal (the previous N+1: GetGoalProgress called in the loop).
	progressByGoal := d.goalProgressByProject(project)

	// Build lookup and progress
	byID := make(map[string]*models.GoalWithProgress, len(goals))
	for _, g := range goals {
		p := progressByGoal[g.ID]
		total, done := p.total, p.done
		var progress float64
		if total > 0 {
			progress = float64(done) / float64(total)
		}
		gwp := &models.GoalWithProgress{
			Goal:       g,
			TotalTasks: total,
			DoneTasks:  done,
			Progress:   progress,
			Children:   []models.GoalWithProgress{},
		}
		byID[g.ID] = gwp
	}

	// Build tree — two passes: first attach children, then collect roots
	// (must not copy values until all children are attached)
	var rootIDs []string
	for _, g := range goals {
		if g.ParentGoalID != nil {
			if parent, ok := byID[*g.ParentGoalID]; ok {
				parent.Children = append(parent.Children, models.GoalWithProgress{}) // placeholder
				continue
			}
		}
		rootIDs = append(rootIDs, g.ID)
	}

	// Re-attach children properly now that we know the tree shape
	// Reset all children and rebuild
	for _, gwp := range byID {
		gwp.Children = nil
	}
	for _, g := range goals {
		if g.ParentGoalID != nil {
			if parent, ok := byID[*g.ParentGoalID]; ok {
				parent.Children = append(parent.Children, *byID[g.ID])
			}
		}
	}

	// Collect roots — must re-read from byID after children are set
	// But children of children won't be correct because we copied...
	// Use a recursive collect instead. Rollup: a goal's total/done counts
	// include its own direct tasks PLUS the aggregated counts of all
	// descendants. This matches the "goal cascade with progress rollup"
	// promise in the README — previously each level only showed its direct
	// tasks, so a project_goal with child agent_goals full of work read as
	// 0/0 and looked empty.
	var collect func(id string) models.GoalWithProgress
	collect = func(id string) models.GoalWithProgress {
		gwp := byID[id]
		result := *gwp
		result.Children = nil
		totalAgg := result.TotalTasks
		doneAgg := result.DoneTasks
		for _, g := range goals {
			if g.ParentGoalID != nil && *g.ParentGoalID == id {
				child := collect(g.ID)
				totalAgg += child.TotalTasks
				doneAgg += child.DoneTasks
				result.Children = append(result.Children, child)
			}
		}
		if result.Children == nil {
			result.Children = []models.GoalWithProgress{}
		}
		result.TotalTasks = totalAgg
		result.DoneTasks = doneAgg
		if totalAgg > 0 {
			result.Progress = float64(doneAgg) / float64(totalAgg)
		} else {
			result.Progress = 0
		}
		return result
	}

	roots := make([]models.GoalWithProgress, 0, len(rootIDs))
	for _, id := range rootIDs {
		roots = append(roots, collect(id))
	}
	return roots, nil
}
