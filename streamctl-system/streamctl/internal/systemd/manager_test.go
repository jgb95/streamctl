package systemd

import (
	"strings"
	"testing"

	"streamctl/internal/db"
)

func TestNormalizedRemoteAndCachePaths(t *testing.T) {
	m := &Manager{
		CacheDir:  "/var/lib/streamctl/cache",
		Remote:    "spaces:btcpp",
		Normalize: true,
	}
	source := "vienna/recordings/edits/stage-two/talk.mp4"

	if got, want := m.normalizedRemoteClipPath(source), "vienna/recordings/normalized/stage-two/talk.mp4"; got != want {
		t.Fatalf("normalized remote path = %q, want %q", got, want)
	}
	if got, want := m.localClipPath(source), "/var/lib/streamctl/cache/normalized/vienna/recordings/edits/stage-two/talk.mp4"; got != want {
		t.Fatalf("local normalized cache path = %q, want %q", got, want)
	}
	paths := m.remoteCachePaths(source)
	if len(paths) != 2 {
		t.Fatalf("expected raw and normalized cleanup paths, got %#v", paths)
	}
	if paths[0] != "/var/lib/streamctl/cache/vienna/recordings/edits/stage-two/talk.mp4" {
		t.Fatalf("raw cleanup path = %q", paths[0])
	}
	if paths[1] != "/var/lib/streamctl/cache/normalized/vienna/recordings/edits/stage-two/talk.mp4" {
		t.Fatalf("normalized cleanup path = %q", paths[1])
	}
}

func TestPrefetchScriptPrefersPreprocessedNormalizedObject(t *testing.T) {
	m := &Manager{
		CacheDir:  "/var/lib/streamctl/cache",
		Remote:    "spaces:btcpp",
		Normalize: true,
	}
	stream := &db.Stream{
		ID:     42,
		Name:   "test",
		Videos: []string{"vienna/recordings/edits/stage-two/talk.mp4"},
	}

	script := m.renderPrefetchScript(stream)
	for _, want := range []string{
		"spaces:btcpp/vienna/recordings/normalized/stage-two/talk.mp4.ready.json",
		"spaces:btcpp/vienna/recordings/normalized/stage-two/talk.mp4",
		"prefetch: fetching preprocessed normalized Spaces object vienna/recordings/normalized/stage-two/talk.mp4",
		"prefetch: preprocessed normalized Spaces object failed verification; falling back to raw vienna/recordings/edits/stage-two/talk.mp4",
		"/var/lib/streamctl/cache/normalized/vienna/recordings/edits/stage-two/talk.mp4",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("prefetch script missing %q\n%s", want, script)
		}
	}
}

func TestRunScriptReportsBTCPPBroadcastLifecycle(t *testing.T) {
	m := &Manager{
		HLSDir: "/var/lib/streamctl/hls", PublicBaseURL: "https://stream.btcpp.dev",
		BTCPPAPIBase: "https://btcpp.dev", BTCPPTokenFile: "/var/lib/streamctl/btcpp-api-token",
		SelfPath: "/run/current-system/sw/bin/cmd",
	}
	stream := &db.Stream{ID: 7, Name: "A talk", Videos: []string{"talk.mp4"}, BTCPPRecordingID: "recording-1"}
	script := m.renderRunScript(stream)
	for _, want := range []string{
		"btcpp-broadcast", "-recording-id 'recording-1'", "-state 'live'",
		"sleep 45", "-state 'ended'", "-state 'failed'",
		"https://stream.btcpp.dev/live/stream-7/index.m3u8",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("run script missing %q\n%s", want, script)
		}
	}
}

func TestStreamServiceEnablesIPAccounting(t *testing.T) {
	m := &Manager{RunUser: "streamctl", VideoDir: "/videos", CacheDir: "/cache", HLSDir: "/hls"}
	unit := m.renderService(&db.Stream{ID: 7, Name: "A talk"})
	if !strings.Contains(unit, "IPAccounting=true") {
		t.Fatalf("stream service did not enable network accounting:\n%s", unit)
	}
}

func TestParseStreamRuntime(t *testing.T) {
	runtime := parseStreamRuntime("ActiveState=active\nResult=success\nIPIngressBytes=123\nIPEgressBytes=456\n")
	if !runtime.Active || runtime.Failed || runtime.Result != "success" || runtime.IngressBytes != 123 || runtime.EgressBytes != 456 {
		t.Fatalf("unexpected runtime: %#v", runtime)
	}

	failed := parseStreamRuntime("ActiveState=failed\nResult=exit-code\nIPIngressBytes=[not set]\nIPEgressBytes=[not set]\n")
	if failed.Active || !failed.Failed || failed.IngressBytes != 0 || failed.EgressBytes != 0 {
		t.Fatalf("unexpected failed runtime: %#v", failed)
	}
}
