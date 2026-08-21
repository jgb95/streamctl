package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOAuthLoginStateIsOneTimeAndExpires(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	if err := database.CreateOAuthLoginState([]byte("state"), "verifier", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	verifier, err := database.ConsumeOAuthLoginState([]byte("state"), now)
	if err != nil || verifier != "verifier" {
		t.Fatalf("consume = %q, %v", verifier, err)
	}
	verifier, err = database.ConsumeOAuthLoginState([]byte("state"), now)
	if err != nil || verifier != "" {
		t.Fatalf("second consume = %q, %v", verifier, err)
	}
}

func TestAuthSessionRoundTripAndExpiry(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	session := AuthSession{
		PersonID: "person-1", DisplayName: "Mara", AccessToken: "access", RefreshToken: "refresh",
		AccessExpiresAt: now.Add(time.Hour), RoleCheckedAt: now, CSRFToken: "csrf",
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), LastSeenAt: now,
	}
	if err := database.CreateAuthSession([]byte("session"), session); err != nil {
		t.Fatal(err)
	}
	got, err := database.FindAuthSession([]byte("session"), now)
	if err != nil || got == nil {
		t.Fatalf("find = %#v, %v", got, err)
	}
	if got.PersonID != session.PersonID || got.CSRFToken != session.CSRFToken || !got.AccessExpiresAt.Equal(session.AccessExpiresAt) {
		t.Fatalf("session = %#v", got)
	}
	got, err = database.FindAuthSession([]byte("session"), session.ExpiresAt)
	if err != nil || got != nil {
		t.Fatalf("expired find = %#v, %v", got, err)
	}
}
