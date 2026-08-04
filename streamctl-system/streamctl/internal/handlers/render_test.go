package handlers

import "testing"

func TestValidateRenderManifest(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{
			name:    "valid",
			payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"/data/source.mp4","overlay":"/data/title.png","audio":{"src":"/data/audio.wav"}}]}]}`,
		},
		{name: "missing jobs", payload: `{"version":1,"jobs":[]}`, wantErr: true},
		{name: "duplicate IDs", payload: `{"version":1,"jobs":[{"id":"same","segments":[{"src":"/a"}]},{"id":"same","segments":[{"src":"/b"}]}]}`, wantErr: true},
		{name: "relative source", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"source.mp4"}]}]}`, wantErr: true},
		{name: "unsupported version", payload: `{"version":2,"jobs":[{"id":"intro","segments":[{"src":"/source.mp4"}]}]}`, wantErr: true},
		{name: "trailing JSON", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"/source.mp4"}]}]} {}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRenderManifest([]byte(tt.payload))
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRenderManifest() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRenderJobIDFromUnit(t *testing.T) {
	if id, ok := renderJobIDFromUnit("streamctl-gpu-render-42.service"); !ok || id != 42 {
		t.Fatalf("renderJobIDFromUnit() = (%d, %v), want (42, true)", id, ok)
	}
	for _, unit := range []string{"streamctl-gpu-foo.service", "streamctl-gpu-render-x.service", "streamctl-gpu-render-0.service"} {
		if _, ok := renderJobIDFromUnit(unit); ok {
			t.Fatalf("renderJobIDFromUnit(%q) unexpectedly succeeded", unit)
		}
	}
}
