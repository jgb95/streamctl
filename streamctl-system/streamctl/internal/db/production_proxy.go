package db

import (
	"database/sql"
	"fmt"
	"strings"
)

type ProductionProxyJob struct {
	ID         int64
	Source     string
	Proxy      string
	Status     string
	Attempts   int
	DurationMS int64
	LastError  string
}

func (db *DB) EnqueueProductionProxyJob(source, proxy string) (ProductionProxyJob, bool, error) {
	source, proxy = strings.TrimSpace(source), strings.TrimSpace(proxy)
	if source == "" || proxy == "" {
		return ProductionProxyJob{}, false, fmt.Errorf("production proxy source and output are required")
	}
	result, err := db.Exec(`
		INSERT INTO production_proxy_jobs (source_object_key, proxy_object_key)
		VALUES (?, ?)
		ON CONFLICT(source_object_key) DO NOTHING
	`, source, proxy)
	if err != nil {
		return ProductionProxyJob{}, false, fmt.Errorf("enqueue production proxy: %w", err)
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		result, err = db.Exec(`
			UPDATE production_proxy_jobs
			SET proxy_object_key = ?, status = 'queued', last_error = '', finished_at = NULL,
			    updated_at = CURRENT_TIMESTAMP
			WHERE source_object_key = ? AND status = 'failed'
		`, proxy, source)
		if err != nil {
			return ProductionProxyJob{}, false, fmt.Errorf("requeue production proxy: %w", err)
		}
		inserted, _ = result.RowsAffected()
	}
	job, err := db.ProductionProxyJobBySource(source)
	return job, inserted > 0, err
}

func (db *DB) ProductionProxyJobBySource(source string) (ProductionProxyJob, error) {
	return scanProductionProxyJob(db.QueryRow(`
		SELECT id, source_object_key, proxy_object_key, status, attempt_count,
		       duration_ms, last_error
		FROM production_proxy_jobs WHERE source_object_key = ?
	`, strings.TrimSpace(source)))
}

func (db *DB) RequeueInterruptedProductionProxyJobs() error {
	_, err := db.Exec(`
		UPDATE production_proxy_jobs
		SET status = 'queued', last_error = 'streamctl restarted while preparing proxy', updated_at = CURRENT_TIMESTAMP
		WHERE status = 'running'
	`)
	return err
}

func (db *DB) ClaimProductionProxyJob() (ProductionProxyJob, error) {
	tx, err := db.Begin()
	if err != nil {
		return ProductionProxyJob{}, err
	}
	defer tx.Rollback()
	job, err := scanProductionProxyJob(tx.QueryRow(`
		SELECT id, source_object_key, proxy_object_key, status, attempt_count,
		       duration_ms, last_error
		FROM production_proxy_jobs WHERE status = 'queued' ORDER BY id LIMIT 1
	`))
	if err != nil {
		return ProductionProxyJob{}, err
	}
	result, err := tx.Exec(`
		UPDATE production_proxy_jobs
		SET status = 'running', attempt_count = attempt_count + 1, started_at = CURRENT_TIMESTAMP,
		    finished_at = NULL, last_error = '', updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'queued'
	`, job.ID)
	if err != nil {
		return ProductionProxyJob{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ProductionProxyJob{}, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return ProductionProxyJob{}, err
	}
	job.Status = "running"
	job.Attempts++
	job.LastError = ""
	return job, nil
}

func (db *DB) FinishProductionProxyJob(id, durationMS int64) error {
	_, err := db.Exec(`
		UPDATE production_proxy_jobs
		SET status = 'finished', duration_ms = ?, last_error = '', finished_at = CURRENT_TIMESTAMP,
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, durationMS, id)
	return err
}

func (db *DB) FailProductionProxyJob(id int64, jobErr error) error {
	detail := "proxy preparation failed"
	if jobErr != nil {
		detail = strings.TrimSpace(jobErr.Error())
	}
	_, err := db.Exec(`
		UPDATE production_proxy_jobs
		SET status = 'failed', last_error = ?, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, detail, id)
	return err
}

type productionProxyScanner interface {
	Scan(...any) error
}

func scanProductionProxyJob(scanner productionProxyScanner) (ProductionProxyJob, error) {
	var job ProductionProxyJob
	err := scanner.Scan(&job.ID, &job.Source, &job.Proxy, &job.Status, &job.Attempts,
		&job.DurationMS, &job.LastError)
	return job, err
}
