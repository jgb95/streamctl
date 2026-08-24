package handlers

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"streamctl/internal/db"
)

func TestValidateRenderManifest(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{
			name:    "valid",
			payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"type":"video","src":"dev26/recordings/source.mp4","overlay":"dev26/assets/title.png","audio":{"src":"dev26/audio/audio.wav"}}]}]}`,
		},
		{name: "missing jobs", payload: `{"version":1,"jobs":[]}`, wantErr: true},
		{name: "duplicate IDs", payload: `{"version":1,"jobs":[{"id":"same","segments":[{"src":"dev26/a"}]},{"id":"same","segments":[{"src":"dev26/b"}]}]}`, wantErr: true},
		{name: "absolute source", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"/source.mp4"}]}]}`, wantErr: true},
		{name: "source without conference", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"source.mp4"}]}]}`, wantErr: true},
		{name: "source traversal", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"dev26/../source.mp4"}]}]}`, wantErr: true},
		{name: "mixed conferences", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"dev26/source.mp4","overlay":"aus25/title.png"}]}]}`, wantErr: true},
		{name: "unsupported version", payload: `{"version":2,"jobs":[{"id":"intro","segments":[{"src":"dev26/source.mp4"}]}]}`, wantErr: true},
		{name: "trailing JSON", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"dev26/source.mp4"}]}]} {}`, wantErr: true},
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

func TestRenderOutputPrefix(t *testing.T) {
	raw := `{"version":1,"jobs":[{"id":"intro","segments":[{"type":"video","src":"dev26/recordings/source.mp4"}]}]}`
	if got, want := renderOutputPrefix(42, raw), "dev26/recordings/renders/42/"; got != want {
		t.Fatalf("renderOutputPrefix() = %q, want %q", got, want)
	}
}

func TestRenderWorkerScriptRoundTrip(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required")
	}
	root := t.TempDir()
	remoteRoot := filepath.Join(root, "remote")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(remoteRoot, "dev26", "recordings"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteRoot, "dev26", "recordings", "source.mp4"), []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	rclone := `#!/usr/bin/env bash
set -euo pipefail
command="$1"
src="${@: -2:1}"
dst="${@: -1}"
local_path() { case "$1" in spaces:test/*) printf '%s/%s' "$REMOTE_ROOT" "${1#spaces:test/}" ;; *) printf '%s' "$1" ;; esac; }
src="$(local_path "$src")"
dst="$(local_path "$dst")"
case "$command" in
  copyto) mkdir -p "$(dirname "$dst")"; cp "$src" "$dst" ;;
  copy) mkdir -p "$dst"; cp -R "$src"/. "$dst" ;;
  *) echo "unsupported fake rclone command: $command" >&2; exit 2 ;;
esac
`
	confRender := `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = validate ]; then exit 0; fi
if [ "$1" != render ]; then exit 2; fi
shift
output=""
while [ $# -gt 0 ]; do
  if [ "$1" = --output ]; then output="$2"; shift 2; else shift; fi
done
mkdir -p "$output"
printf 'rendered' > "$output/intro.mp4"
`
	for name, body := range map[string]string{"rclone": rclone, "conf-render": confRender} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(manifest, []byte(`{"version":1,"jobs":[{"id":"intro","segments":[{"type":"video","src":"dev26/recordings/source.mp4"}]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "render-from-spaces.py")
	if err := os.WriteFile(script, []byte(renderWorkerScript()), 0755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "output", "42")
	cmd := exec.Command(python, script, manifest, output, filepath.Join(root, "work"))
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"REMOTE_ROOT="+remoteRoot,
		"SPACES_REMOTE=spaces:test",
		"CONF_RENDER_COMMAND="+filepath.Join(binDir, "conf-render"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render worker failed: %v\n%s", err, out)
	}
	for _, name := range []string{"intro.mp4", "ready.json"} {
		path := filepath.Join(remoteRoot, "dev26", "recordings", "renders", "42", name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("uploaded %s: %v", name, err)
		}
	}
}

func TestShouldRequeueMissingRenderJob(t *testing.T) {
	now := time.Now()
	old := now.Add(-renderSubmissionStaleAfter)
	recent := now.Add(-renderSubmissionStaleAfter / 2)

	if !shouldRequeueMissingRenderJob(db.RenderJobQueueItem{Status: "running", StartedAt: &old}, true, now) {
		t.Fatal("stale missing render was not selected for requeue")
	}
	for _, item := range []db.RenderJobQueueItem{
		{Status: "running", StartedAt: &recent},
		{Status: "queued", StartedAt: &old},
		{Status: "running"},
	} {
		if shouldRequeueMissingRenderJob(item, true, now) {
			t.Fatalf("render unexpectedly selected for requeue: %#v", item)
		}
	}
	if shouldRequeueMissingRenderJob(db.RenderJobQueueItem{Status: "running", StartedAt: &old}, false, now) {
		t.Fatal("unreliable worker state selected a render for requeue")
	}
}

func TestReconcileUnavailableRenderQueue(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("keynote", `{"version":1,"jobs":[{"id":"keynote","segments":[{"src":"/data/keynote.mp4"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueRunning(item.ID, renderUnitName(item.ID)); err != nil {
		t.Fatal(err)
	}

	h := &Handler{DB: database}
	h.reconcileUnavailableRenderQueue(gpuWorkerView{Managed: true, Status: "not found"})

	got, err := database.GetRenderQueueItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || got.UnitName != "" {
		t.Fatalf("missing managed worker did not requeue render: %#v", got)
	}
}

func TestReconcileUnavailableRenderQueueLeavesStartingWorkerAlone(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("keynote", `{"version":1,"jobs":[{"id":"keynote","segments":[{"src":"/data/keynote.mp4"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueRunning(item.ID, renderUnitName(item.ID)); err != nil {
		t.Fatal(err)
	}

	h := &Handler{DB: database}
	h.reconcileUnavailableRenderQueue(gpuWorkerView{Managed: true, Status: "starting"})

	got, err := database.GetRenderQueueItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("starting worker requeued render: %#v", got)
	}
}
