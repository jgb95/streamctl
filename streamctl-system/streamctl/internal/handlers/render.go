package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"streamctl/internal/db"
)

const maxRenderManifestBytes = 2 << 20

type renderManifestEnvelope struct {
	Version  int             `json:"version"`
	Jobs     []renderJobStub `json:"jobs"`
	Settings json.RawMessage `json:"settings"`
}

type renderJobStub struct {
	ID       string              `json:"id"`
	Segments []renderSegmentStub `json:"segments"`
}

type renderSegmentStub struct {
	Src     string `json:"src"`
	Overlay string `json:"overlay"`
	Audio   *struct {
		Src string `json:"src"`
	} `json:"audio"`
}

type renderQueueItemView struct {
	ID           int64
	Name         string
	UnitName     string
	Status       string
	AttemptCount int
	LastError    string
	CreatedAt    string
	StartedAt    string
	FinishedAt   string
}

type renderJobsView struct {
	Items     []renderQueueItemView
	Queued    int
	Running   int
	Failed    int
	Finished  int
	Cancelled int
	Error     string
}

func validateRenderManifest(raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("manifest is required")
	}
	if len(raw) > maxRenderManifestBytes {
		return fmt.Errorf("manifest exceeds %d bytes", maxRenderManifestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var manifest renderManifestEnvelope
	if err := dec.Decode(&manifest); err != nil {
		return fmt.Errorf("invalid manifest JSON: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return errors.New("manifest must contain one JSON object")
	}
	if manifest.Version != 1 {
		return fmt.Errorf("unsupported manifest version %d", manifest.Version)
	}
	if len(manifest.Jobs) == 0 {
		return errors.New("manifest must contain at least one job")
	}
	seen := map[string]bool{}
	for _, job := range manifest.Jobs {
		id := strings.TrimSpace(job.ID)
		if id == "" {
			return errors.New("every manifest job must have an id")
		}
		if seen[id] {
			return fmt.Errorf("duplicate manifest job id %q", id)
		}
		seen[id] = true
		if len(job.Segments) == 0 {
			return fmt.Errorf("manifest job %q must contain at least one segment", id)
		}
		for _, segment := range job.Segments {
			for _, source := range []string{segment.Src, segment.Overlay} {
				if source != "" && !strings.HasPrefix(source, "/") {
					return fmt.Errorf("manifest source %q must be an absolute worker path", source)
				}
			}
			if segment.Audio != nil && segment.Audio.Src != "" && !strings.HasPrefix(segment.Audio.Src, "/") {
				return fmt.Errorf("manifest audio source %q must be an absolute worker path", segment.Audio.Src)
			}
		}
	}
	return nil
}

func (h *Handler) renderJobs(w http.ResponseWriter, r *http.Request) {
	items, err := h.DB.ListVisibleRenderQueueItems(50)
	view := renderJobsView{}
	if err != nil {
		view.Error = err.Error()
	} else {
		for _, item := range items {
			view.Items = append(view.Items, renderQueueItemView{
				ID: item.ID, Name: item.Name, UnitName: item.UnitName, Status: item.Status,
				AttemptCount: item.AttemptCount, LastError: item.LastError,
				CreatedAt: item.CreatedAt.Format("2006-01-02 15:04"),
				StartedAt: formatOptionalTime(item.StartedAt), FinishedAt: formatOptionalTime(item.FinishedAt),
			})
		}
	}
	if counts, countErr := h.DB.RenderQueueStatusCounts(); countErr != nil {
		if view.Error == "" {
			view.Error = countErr.Error()
		}
	} else {
		view.Queued, view.Running, view.Failed = counts["queued"], counts["running"], counts["failed"]
		view.Finished, view.Cancelled = counts["finished"], counts["cancelled"]
	}
	h.render(w, r, "render_jobs.html", view)
}

func (h *Handler) renderJobCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRenderManifestBytes+64*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	raw := []byte(r.FormValue("manifest"))
	if err := validateRenderManifest(raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "conf-render job"
	}
	if _, err := h.DB.EnqueueRenderJob(name, string(raw)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go h.dispatchGPUQueueOnce(context.Background())
	http.Redirect(w, r, "/render-jobs", http.StatusSeeOther)
}

func (h *Handler) renderJobRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/render-jobs/retry/"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if err := h.DB.ResetRenderJobForRetry(id, "manual retry"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	go h.dispatchGPUQueueOnce(context.Background())
	http.Redirect(w, r, "/render-jobs", http.StatusSeeOther)
}

func renderUnitName(id int64) string  { return fmt.Sprintf("streamctl-gpu-render-%d.service", id) }
func renderWorkspace(id int64) string { return fmt.Sprintf("/root/streamctl-render-jobs/%d", id) }

func remoteSSHInput(ctx context.Context, host, remoteCommand, input string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(host, remoteCommand)...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *Handler) startRemoteRender(ctx context.Context, host string, item db.RenderJobQueueItem) (string, string, error) {
	unit, workspace := renderUnitName(item.ID), renderWorkspace(item.ID)
	stage := "umask 077; mkdir -p " + shellQuote(workspace) + "; rm -f " + shellQuote(workspace+"/result") + " " + shellQuote(workspace+"/exit-code") + "; cat > " + shellQuote(workspace+"/manifest.json")
	if out, err := remoteSSHInput(ctx, host, stage, item.ManifestJSON); err != nil {
		return unit, out, fmt.Errorf("stage manifest: %w", err)
	}
	command := strings.TrimSpace(h.RenderWorkerCommand)
	if command == "" {
		return unit, "", errors.New("render worker command is not configured")
	}
	manifest := workspace + "/manifest.json"
	output := strings.TrimRight(h.RenderOutputDir, "/") + "/" + strconv.FormatInt(item.ID, 10)
	validateCommand := command + " validate " + shellQuote(manifest)
	if out, err := remoteSSH(ctx, host, validateCommand); err != nil {
		_, _ = remoteSSH(context.Background(), host, "rm -rf -- "+shellQuote(workspace))
		return unit, out, fmt.Errorf("conf-render validation: %w", err)
	}
	renderInvocation := command + " render " + shellQuote(manifest) + " --output " + shellQuote(output) + " --work-dir " + shellQuote(workspace+"/work") + " --overwrite"
	renderCommand := "set +e; mkdir -p " + shellQuote(output) + "; " + renderInvocation + "; rc=$?; printf '%s\\n' \"$rc\" > " + shellQuote(workspace+"/exit-code") + "; if [ \"$rc\" -eq 0 ]; then printf 'success\\n' > " + shellQuote(workspace+"/result") + "; else printf 'failed\\n' > " + shellQuote(workspace+"/result") + "; fi; exit \"$rc\""
	remote := "systemd-run --unit=" + shellQuote(strings.TrimSuffix(unit, ".service")) + " --collect --property=Type=exec --property=TimeoutStartSec=48h /bin/sh -lc " + shellQuote(renderCommand)
	out, err := remoteSSH(ctx, host, remote)
	return unit, out, err
}

func (h *Handler) dispatchNextRender(ctx context.Context, host string) bool {
	item, err := h.DB.NextQueuedRenderJob()
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("loading next queued render job failed: %v", err)
		}
		return false
	}
	unit := renderUnitName(item.ID)
	if err := h.DB.MarkRenderQueueRunning(item.ID, unit); err != nil {
		log.Printf("claiming render job %d failed: %v", item.ID, err)
		return true
	}
	unit, out, err := h.startRemoteRender(ctx, host, *item)
	if err != nil {
		errText := strings.TrimSpace(fmt.Sprintf("%v: %s", err, out))
		log.Printf("render job %d submission failed: %s", item.ID, errText)
		_ = h.DB.MarkRenderQueueFinished(item.ID, "failed", errText)
		return true
	}
	go h.monitorRenderJob(item.ID, unit, host)
	return true
}

func renderJobIDFromUnit(unit string) (int64, bool) {
	value := strings.TrimSuffix(strings.TrimPrefix(unit, "streamctl-gpu-render-"), ".service")
	if value == unit {
		return 0, false
	}
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0
}

func (h *Handler) reconcileRenderJobs(ctx context.Context, host string, jobs []gpuJobView) {
	seen := make(map[int64]bool)
	for _, job := range jobs {
		id, ok := renderJobIDFromUnit(job.UnitName)
		if !ok {
			continue
		}
		seen[id] = true
		if !isTerminalGPUJob(job) {
			continue
		}
		item, err := h.DB.GetRenderQueueItem(id)
		if err != nil || item.Status != "running" {
			continue
		}
		status, message := "finished", ""
		if job.ActiveState == "failed" || (job.Result != "" && job.Result != "success") {
			status = "failed"
			full := h.gpuJob(ctx, host, job.UnitName, true)
			message = firstNonEmptyString(full.Journal, full.Error, full.Result)
		}
		if err := h.DB.MarkRenderQueueFinished(id, status, message); err != nil {
			log.Printf("reconciling render job %d failed: %v", id, err)
			continue
		}
		_, _ = remoteSSH(context.Background(), host, "rm -rf -- "+shellQuote(renderWorkspace(id)))
	}
	running, err := h.DB.ListOpenRenderQueueItems(1000)
	if err != nil {
		log.Printf("listing running render jobs for reconciliation failed: %v", err)
		return
	}
	for _, item := range running {
		if item.Status != "running" || seen[item.ID] {
			continue
		}
		status, exitCode, found := h.remoteRenderResult(ctx, host, item.ID)
		if !found {
			continue
		}
		message := ""
		if status != "finished" {
			message = "conf-render exited with status " + exitCode
		}
		if err := h.DB.MarkRenderQueueFinished(item.ID, status, message); err != nil {
			log.Printf("reconciling durable render result %d failed: %v", item.ID, err)
			continue
		}
		_, _ = remoteSSH(context.Background(), host, "rm -rf -- "+shellQuote(renderWorkspace(item.ID)))
	}
}

func (h *Handler) remoteRenderResult(ctx context.Context, host string, id int64) (string, string, bool) {
	workspace := renderWorkspace(id)
	out, err := remoteSSH(ctx, host, "if [ -f "+shellQuote(workspace+"/result")+" ]; then cat "+shellQuote(workspace+"/result")+"; cat "+shellQuote(workspace+"/exit-code")+" 2>/dev/null || true; fi")
	if err != nil {
		return "", "", false
	}
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return "", "", false
	}
	exitCode := "unknown"
	if len(lines) > 1 {
		exitCode = lines[1]
	}
	if lines[0] == "success" {
		return "finished", exitCode, true
	}
	return "failed", exitCode, true
}

func (h *Handler) monitorRenderJob(id int64, unit, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), 48*time.Hour)
	defer cancel()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = h.DB.MarkRenderQueueFinished(id, "failed", "render monitor timed out")
			return
		case <-ticker.C:
			if status, exitCode, found := h.remoteRenderResult(ctx, host, id); found {
				message := ""
				if status != "finished" {
					message = "conf-render exited with status " + exitCode
				}
				_ = h.DB.MarkRenderQueueFinished(id, status, message)
				_, _ = remoteSSH(context.Background(), host, "rm -rf -- "+shellQuote(renderWorkspace(id)))
				go h.dispatchGPUQueueOnce(context.Background())
				return
			}
			job := h.gpuJob(ctx, host, unit, false)
			if !isTerminalGPUJob(job) {
				continue
			}
			status, message := "finished", ""
			if job.ActiveState == "failed" || (job.Result != "" && job.Result != "success") {
				status = "failed"
				full := h.gpuJob(ctx, host, unit, true)
				message = firstNonEmptyString(full.Journal, full.Error, full.Result)
			}
			_ = h.DB.MarkRenderQueueFinished(id, status, message)
			_, _ = remoteSSH(context.Background(), host, "rm -rf -- "+shellQuote(renderWorkspace(id)))
			go h.dispatchGPUQueueOnce(context.Background())
			return
		}
	}
}
