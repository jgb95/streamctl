package db

import (
	"database/sql"
	"time"
)

type AuthSession struct {
	PersonID        string
	DisplayName     string
	AccessToken     string
	RefreshToken    string
	AccessExpiresAt time.Time
	RoleCheckedAt   time.Time
	CSRFToken       string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	LastSeenAt      time.Time
}

func (db *DB) CreateOAuthLoginState(stateHash []byte, verifier string, now, expiresAt time.Time) error {
	_, err := db.Exec(`
		INSERT INTO oauth_login_states (state_hash, code_verifier, expires_at, created_at)
		VALUES (?, ?, ?, ?)
	`, stateHash, verifier, expiresAt.Unix(), now.Unix())
	return err
}

func (db *DB) ConsumeOAuthLoginState(stateHash []byte, now time.Time) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var verifier string
	var expiresAt int64
	err = tx.QueryRow(`SELECT code_verifier, expires_at FROM oauth_login_states WHERE state_hash = ?`, stateHash).Scan(&verifier, &expiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	if _, err := tx.Exec(`DELETE FROM oauth_login_states WHERE state_hash = ?`, stateHash); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if expiresAt <= now.Unix() {
		return "", nil
	}
	return verifier, nil
}

func (db *DB) CreateAuthSession(sessionHash []byte, session AuthSession) error {
	_, err := db.Exec(`
		INSERT INTO auth_sessions (
			session_hash, person_id, display_name, access_token, refresh_token,
			access_expires_at, role_checked_at, csrf_token, created_at, expires_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sessionHash, session.PersonID, session.DisplayName, session.AccessToken, session.RefreshToken,
		session.AccessExpiresAt.Unix(), session.RoleCheckedAt.Unix(), session.CSRFToken,
		session.CreatedAt.Unix(), session.ExpiresAt.Unix(), session.LastSeenAt.Unix())
	return err
}

func (db *DB) FindAuthSession(sessionHash []byte, now time.Time) (*AuthSession, error) {
	var session AuthSession
	var accessExpiresAt, roleCheckedAt, createdAt, expiresAt, lastSeenAt int64
	err := db.QueryRow(`
		SELECT person_id, display_name, access_token, refresh_token, access_expires_at,
		       role_checked_at, csrf_token, created_at, expires_at, last_seen_at
		FROM auth_sessions
		WHERE session_hash = ? AND expires_at > ?
	`, sessionHash, now.Unix()).Scan(
		&session.PersonID, &session.DisplayName, &session.AccessToken, &session.RefreshToken,
		&accessExpiresAt, &roleCheckedAt, &session.CSRFToken, &createdAt, &expiresAt, &lastSeenAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	session.AccessExpiresAt = time.Unix(accessExpiresAt, 0)
	session.RoleCheckedAt = time.Unix(roleCheckedAt, 0)
	session.CreatedAt = time.Unix(createdAt, 0)
	session.ExpiresAt = time.Unix(expiresAt, 0)
	session.LastSeenAt = time.Unix(lastSeenAt, 0)
	return &session, nil
}

func (db *DB) UpdateAuthSession(sessionHash []byte, session AuthSession) error {
	_, err := db.Exec(`
		UPDATE auth_sessions
		SET display_name = ?, access_token = ?, refresh_token = ?, access_expires_at = ?,
		    role_checked_at = ?, expires_at = ?, last_seen_at = ?
		WHERE session_hash = ?
	`, session.DisplayName, session.AccessToken, session.RefreshToken, session.AccessExpiresAt.Unix(),
		session.RoleCheckedAt.Unix(), session.ExpiresAt.Unix(), session.LastSeenAt.Unix(), sessionHash)
	return err
}

func (db *DB) DeleteAuthSession(sessionHash []byte) error {
	_, err := db.Exec(`DELETE FROM auth_sessions WHERE session_hash = ?`, sessionHash)
	return err
}

func (db *DB) CleanupAuthState(now time.Time) error {
	if _, err := db.Exec(`DELETE FROM oauth_login_states WHERE expires_at <= ?`, now.Unix()); err != nil {
		return err
	}
	_, err := db.Exec(`DELETE FROM auth_sessions WHERE expires_at <= ?`, now.Unix())
	return err
}
