package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Profiles are slim identity cards: slug, name, role, and skills (for discovery
// via find_profiles). The agent-OS execution fields (context_pack, vault_paths,
// pool_size, exit_prompt, etc.) were removed with the spawn subsystem; their
// columns may still exist in the table but are no longer read or written.
const profileColumns = "id, slug, name, role, skills, project, org_id, created_at, updated_at"

func scanProfile(row interface{ Scan(...any) error }) (models.Profile, error) {
	var p models.Profile
	err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.Role, &p.Skills, &p.Project, &p.OrgID, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (d *DB) RegisterProfile(project, slug, name, role, skills string) (*models.Profile, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	if skills == "" {
		skills = "[]"
	}

	// Upsert: update if exists
	existing, err := scanProfile(d.conn.QueryRow(
		"SELECT "+profileColumns+" FROM profiles WHERE slug = ? AND project = ?",
		slug, project,
	))

	if err == sql.ErrNoRows {
		p := &models.Profile{
			ID:        uuid.New().String(),
			Slug:      slug,
			Name:      name,
			Role:      role,
			Skills:    skills,
			Project:   project,
			CreatedAt: now,
			UpdatedAt: now,
		}
		_, err := d.writerExec(
			"INSERT INTO profiles (id, slug, name, role, skills, project, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			p.ID, p.Slug, p.Name, p.Role, p.Skills, p.Project, p.CreatedAt, p.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("insert profile: %w", err)
		}
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query profile: %w", err)
	}

	// Update existing — PATCH semantics. An empty parameter preserves the
	// existing value instead of wiping it.
	if name != "" {
		existing.Name = name
	}
	if role != "" {
		existing.Role = role
	}
	if skills != "" && skills != "[]" {
		existing.Skills = skills
	}
	existing.UpdatedAt = now

	_, err = d.writerExec(
		"UPDATE profiles SET name = ?, role = ?, skills = ?, updated_at = ? WHERE slug = ? AND project = ?",
		existing.Name, existing.Role, existing.Skills, now, slug, project,
	)
	if err != nil {
		return nil, fmt.Errorf("update profile: %w", err)
	}
	return &existing, nil
}

func (d *DB) GetProfile(project, slug string) (*models.Profile, error) {
	p, err := scanProfile(d.ro().QueryRow(
		"SELECT "+profileColumns+" FROM profiles WHERE slug = ? AND project = ?",
		slug, project,
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get profile: %w", err)
	}
	return &p, nil
}

func (d *DB) ListProfiles(project string) ([]models.Profile, error) {
	rows, err := d.ro().Query(
		"SELECT "+profileColumns+" FROM profiles WHERE project = ? ORDER BY slug",
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var profiles []models.Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

func (d *DB) ListAllProfiles() ([]models.Profile, error) {
	rows, err := d.ro().Query(
		"SELECT " + profileColumns + " FROM profiles ORDER BY project, slug",
	)
	if err != nil {
		return nil, fmt.Errorf("list all profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var profiles []models.Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// DeleteProfile removes a profile by slug and project.
func (d *DB) DeleteProfile(project, slug string) error {
	_, err := d.writerExec("DELETE FROM profiles WHERE slug = ? AND project = ?", slug, project)
	return err
}

// FindProfilesBySkillTag returns profiles whose skills JSON contains the given tag.
func (d *DB) FindProfilesBySkillTag(project, tag string) ([]models.Profile, error) {
	// SQLite JSON: search in the skills JSON array for objects containing the tag
	rows, err := d.ro().Query(
		`SELECT `+profileColumns+` FROM profiles
		 WHERE project = ? AND skills LIKE ?
		 ORDER BY slug`,
		project, "%"+tag+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("find profiles by skill tag: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var profiles []models.Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}
