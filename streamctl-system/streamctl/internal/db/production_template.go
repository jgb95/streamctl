package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ProductionTemplate is a reusable, conference-scoped conf-render segment
// sequence. TemplateJSON contains the version, settings, and segments.
type ProductionTemplate struct {
	ID         int64
	Conference string
	Name       string
	JSON       string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (db *DB) ListProductionTemplates(conference string) ([]ProductionTemplate, error) {
	rows, err := db.Query(`
		SELECT id, conference, name, template_json, created_at, updated_at
		FROM production_templates
		WHERE conference = ?
		ORDER BY updated_at DESC, id DESC
	`, strings.TrimSpace(conference))
	if err != nil {
		return nil, fmt.Errorf("list production templates: %w", err)
	}
	defer rows.Close()
	var templates []ProductionTemplate
	for rows.Next() {
		item, err := scanProductionTemplate(rows)
		if err != nil {
			return nil, err
		}
		templates = append(templates, item)
	}
	return templates, rows.Err()
}

func (db *DB) ProductionTemplate(id int64, conference string) (ProductionTemplate, error) {
	return scanProductionTemplate(db.QueryRow(`
		SELECT id, conference, name, template_json, created_at, updated_at
		FROM production_templates
		WHERE id = ? AND conference = ?
	`, id, strings.TrimSpace(conference)))
}

func (db *DB) CreateProductionTemplate(conference, name, definition string) (int64, error) {
	conference, name = strings.TrimSpace(conference), strings.TrimSpace(name)
	if conference == "" || name == "" || strings.TrimSpace(definition) == "" {
		return 0, fmt.Errorf("conference, template name, and definition are required")
	}
	result, err := db.Exec(`
		INSERT INTO production_templates (conference, name, template_json)
		VALUES (?, ?, ?)
	`, conference, name, definition)
	if err != nil {
		return 0, fmt.Errorf("create production template: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read production template ID: %w", err)
	}
	return id, nil
}

func (db *DB) UpdateProductionTemplate(id int64, conference, name, definition string) error {
	conference, name = strings.TrimSpace(conference), strings.TrimSpace(name)
	if id <= 0 || conference == "" || name == "" || strings.TrimSpace(definition) == "" {
		return fmt.Errorf("template ID, conference, name, and definition are required")
	}
	result, err := db.Exec(`
		UPDATE production_templates
		SET name = ?, template_json = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND conference = ?
	`, name, definition, id, conference)
	if err != nil {
		return fmt.Errorf("update production template: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) DeleteProductionTemplate(id int64, conference string) error {
	result, err := db.Exec(`DELETE FROM production_templates WHERE id = ? AND conference = ?`, id, strings.TrimSpace(conference))
	if err != nil {
		return fmt.Errorf("delete production template: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

type productionTemplateScanner interface {
	Scan(...any) error
}

func scanProductionTemplate(scanner productionTemplateScanner) (ProductionTemplate, error) {
	var item ProductionTemplate
	if err := scanner.Scan(&item.ID, &item.Conference, &item.Name, &item.JSON, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return ProductionTemplate{}, err
	}
	return item, nil
}
