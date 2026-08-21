package btcppclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestClientListsCandidatesAndNeverPlacesTokenInURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/conferences/dev26/recording-candidates" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("query leaked data: %q", r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"talk_id": "talk-1", "title": "A talk", "eligible": true}}})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret-token", HTTPClient: server.Client()}
	candidates, err := client.RecordingCandidates(context.Background(), "dev26")
	if err != nil || len(candidates) != 1 || candidates[0].TalkID != "talk-1" {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
}

func TestTokenFileRejectsBroadPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := TokenFromFile(path); err == nil {
		t.Fatal("accepted group/world-readable token")
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if token, err := TokenFromFile(path); err != nil || token != "secret-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestClientUpdatesBroadcastHeartbeat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/recordings/recording-1/broadcast" || r.Method != http.MethodPut {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var update BroadcastUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Fatal(err)
		}
		if update.State != "live" || update.HLSURL != "https://stream.btcpp.dev/live/stream-7/index.m3u8" {
			t.Fatalf("unexpected update: %#v", update)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"state": "live", "is_live": true}})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret-token", HTTPClient: server.Client()}
	broadcast, err := client.PutBroadcast(context.Background(), "recording-1", BroadcastUpdate{
		State: "live", HLSURL: "https://stream.btcpp.dev/live/stream-7/index.m3u8",
	})
	if err != nil || broadcast.State != "live" || !broadcast.IsLive {
		t.Fatalf("broadcast=%+v err=%v", broadcast, err)
	}
}
