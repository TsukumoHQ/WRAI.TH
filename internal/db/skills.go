package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// UpsertSkill creates or updates a skill in the catalog.
func (d *DB) UpsertSkill(project, name, description, tags string) (*models.Skill, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	if tags == "" {
		tags = "[]"
	}

	var existingID string
	err := d.conn.QueryRow(`SELECT id FROM skills WHERE project = ? AND name = ?`, project, name).Scan(&existingID)
	if err == sql.ErrNoRows {
		id := uuid.New().String()
		_, err := d.conn.Exec(`INSERT INTO skills (id, project, name, description, tags, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, project, name, description, tags, now)
		if err != nil {
			return nil, fmt.Errorf("insert skill: %w", err)
		}
		return &models.Skill{ID: id, Project: project, Name: name, Description: description, Tags: tags, CreatedAt: now}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check skill: %w", err)
	}

	_, err = d.conn.Exec(`UPDATE skills SET description=?, tags=? WHERE id=?`, description, tags, existingID)
	if err != nil {
		return nil, fmt.Errorf("update skill: %w", err)
	}
	return &models.Skill{ID: existingID, Project: project, Name: name, Description: description, Tags: tags, CreatedAt: now}, nil
}

// ListSkills returns all skills for a project.
func (d *DB) ListSkills(project string) ([]models.Skill, error) {
	rows, err := d.ro().Query(`SELECT id, project, name, description, tags, created_at FROM skills WHERE project = ? ORDER BY name LIMIT 200`, project)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	defer rows.Close()

	var result []models.Skill
	for rows.Next() {
		var s models.Skill
		if err := rows.Scan(&s.ID, &s.Project, &s.Name, &s.Description, &s.Tags, &s.CreatedAt); err != nil {
			continue
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// DeleteSkill removes a skill and its profile links.
func (d *DB) DeleteSkill(project, name string) error {
	// Get skill ID first
	var id string
	err := d.conn.QueryRow("SELECT id FROM skills WHERE project = ? AND name = ?", project, name).Scan(&id)
	if err != nil {
		return err
	}
	_, _ = d.conn.Exec("DELETE FROM profile_skills WHERE skill_id = ?", id)
	_, err = d.conn.Exec("DELETE FROM skills WHERE id = ?", id)
	return err
}

// LinkProfileSkill creates a link between a profile and a skill.
func (d *DB) LinkProfileSkill(profileID, skillID, proficiency string) error {
	if proficiency == "" {
		proficiency = "capable"
	}
	_, err := d.conn.Exec(`INSERT OR REPLACE INTO profile_skills (profile_id, skill_id, proficiency) VALUES (?, ?, ?)`,
		profileID, skillID, proficiency)
	return err
}

// UnlinkProfileSkill removes the link between a profile and a skill.
func (d *DB) UnlinkProfileSkill(profileID, skillID string) error {
	_, err := d.conn.Exec(`DELETE FROM profile_skills WHERE profile_id = ? AND skill_id = ?`, profileID, skillID)
	return err
}

// GetProfileSkillLinks returns all skills linked to a profile with proficiency info.
func (d *DB) GetProfileSkillLinks(profileID string) ([]map[string]any, error) {
	rows, err := d.ro().Query(
		`SELECT s.id, s.name, s.description, COALESCE(ps.proficiency, 'capable')
		 FROM skills s
		 JOIN profile_skills ps ON ps.skill_id = s.id
		 WHERE ps.profile_id = ?
		 ORDER BY s.name`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id, name, desc, prof string
		if err := rows.Scan(&id, &name, &desc, &prof); err != nil {
			continue
		}
		result = append(result, map[string]any{"id": id, "name": name, "description": desc, "proficiency": prof})
	}
	return result, rows.Err()
}

// GetSkillByName returns a skill by project + name.
func (d *DB) GetSkillByName(project, name string) (*models.Skill, error) {
	var s models.Skill
	err := d.ro().QueryRow(`SELECT id, project, name, description, tags, created_at FROM skills WHERE project = ? AND name = ?`, project, name).
		Scan(&s.ID, &s.Project, &s.Name, &s.Description, &s.Tags, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// FindProfilesBySkill returns profiles linked to a specific skill via the structured registry.
func (d *DB) FindProfilesBySkill(project, skillName string) ([]models.Profile, error) {
	// Column list MUST match scanProfile's destinations. It previously selected a
	// richer 15-column set (context_pack, soul_keys, …) that scanProfile doesn't
	// read, so every row failed to scan and was silently skipped — making the
	// structured skill→profile routing dead-on-arrival the moment any profile_skills
	// link existed. Select exactly profileColumns, aliased to p.
	rows, err := d.ro().Query(
		`SELECT p.id, p.slug, p.name, p.role, p.skills, p.project, p.org_id, p.created_at, p.updated_at
		 FROM profiles p
		 JOIN profile_skills ps ON ps.profile_id = p.id
		 JOIN skills s ON s.id = ps.skill_id
		 WHERE p.project = ? AND s.name = ?
		 ORDER BY CASE ps.proficiency WHEN 'expert' THEN 0 WHEN 'capable' THEN 1 ELSE 2 END, p.slug`,
		project, skillName,
	)
	if err != nil {
		return nil, fmt.Errorf("find profiles by skill: %w", err)
	}
	defer rows.Close()

	var profiles []models.Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			// Surface scan errors instead of silently skipping — a swallowed
			// mismatch here is exactly what killed routing before.
			return nil, fmt.Errorf("scan profile by skill: %w", err)
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// FindBestProfileForSkill returns the best profile for a skill (expert first, then capable).
func (d *DB) FindBestProfileForSkill(project, skillName string) (*models.Profile, error) {
	profiles, err := d.FindProfilesBySkill(project, skillName)
	if err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		// Fallback to LIKE search on skills JSON
		fallback, err := d.FindProfilesBySkillTag(project, skillName)
		if err != nil || len(fallback) == 0 {
			return nil, nil
		}
		return &fallback[0], nil
	}
	return &profiles[0], nil
}

// GetSkillProfileLinks returns profiles linked to a skill with proficiency info.
func (d *DB) GetSkillProfileLinks(project, skillName string) ([]map[string]any, error) {
	rows, err := d.ro().Query(
		`SELECT p.slug, p.name, ps.proficiency FROM profiles p
		 JOIN profile_skills ps ON ps.profile_id = p.id
		 JOIN skills s ON s.id = ps.skill_id
		 WHERE p.project = ? AND s.name = ?
		 ORDER BY ps.proficiency, p.slug`,
		project, skillName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var slug, name, prof string
		if err := rows.Scan(&slug, &name, &prof); err != nil {
			continue
		}
		result = append(result, map[string]any{"slug": slug, "name": name, "proficiency": prof})
	}
	return result, rows.Err()
}
