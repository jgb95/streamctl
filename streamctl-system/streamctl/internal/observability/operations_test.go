package observability

import (
	"testing"

	"streamctl/internal/db"
)

func TestEnabledRemoteDestinations(t *testing.T) {
	endpoints := []db.Endpoint{
		{Name: "YouTube", Type: "youtube_hls", Enabled: true},
		{Name: "X", Type: "rtmp", Enabled: true},
		{Name: "Disabled", Type: "rtmp", Enabled: false},
	}
	destinations := enabledRemoteDestinations(endpoints)
	if len(destinations) != 2 || destinations[0].Name != "YouTube" || destinations[1].Name != "X" {
		t.Fatalf("unexpected enabled destinations: %#v", destinations)
	}
}
