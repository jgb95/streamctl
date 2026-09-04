package handlers

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"streamctl/internal/db"
)

const productionProxyDirectory = "workspace/proxies"

func (h *Handler) productionProxyPrepare(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	target := strings.TrimSpace(r.FormValue("target"))
	if !validProductionConference(conference) {
		http.Error(w, "invalid conference", http.StatusBadRequest)
		return
	}
	if h.DB == nil || strings.TrimSpace(h.Remote) == "" {
		http.Error(w, "media preparation is not configured", http.StatusServiceUnavailable)
		return
	}
	sources, err := h.productionProxyTargetSources(r.Context(), conference, target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	existing, inventoryErr := h.productionProxyArtifactInventory(r.Context(), conference)
	if inventoryErr != nil {
		http.Error(w, inventoryErr.Error(), http.StatusBadGateway)
		return
	}
	queued := 0
	for _, source := range sources {
		proxy := productionProxyObjectKey(conference, source)
		if proxy == "" || existing[proxy] {
			continue
		}
		_, inserted, err := h.DB.EnqueueProductionProxyJob(source.Path, proxy)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if inserted {
			queued++
		}
	}
	if queued > 0 {
		go h.dispatchProductionProxyQueue(context.Background())
	}
	message := "No new editing proxy jobs were needed."
	if queued == 1 {
		message = "Queued 1 new editing proxy job."
	} else if queued > 1 {
		message = fmt.Sprintf("Queued %d new editing proxy jobs.", queued)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"queued":  queued,
		"status":  "queued",
		"message": message,
	})
}

func (h *Handler) productionProxyTargetSources(ctx context.Context, conference, target string) ([]mediaFile, error) {
	target = strings.TrimSpace(strings.ReplaceAll(target, "\\", "/"))
	root := conference + "/recordings/"
	if strings.HasSuffix(target, "/") {
		prefix, err := cleanSpacesPrefix(target)
		if err != nil || prefix == root || !strings.HasPrefix(prefix, root) || isProductionWorkspacePath(prefix) {
			return nil, fmt.Errorf("choose a source folder inside this conference's recordings folder")
		}
		return h.logicalMediaSourcesRecursive(ctx, prefix)
	}
	objectKey, err := validateRenderObjectKey(target)
	if err != nil || !strings.HasPrefix(objectKey, root) || isProductionWorkspacePath(objectKey) || !isVideoFile(objectKey) {
		return nil, fmt.Errorf("choose a source video inside this conference's recordings folder")
	}
	source, err := h.logicalMediaSource(ctx, objectKey)
	if err != nil {
		return nil, err
	}
	return []mediaFile{source}, nil
}

func (h *Handler) productionProxyRequeue(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid media preparation job", http.StatusBadRequest)
		return
	}
	if err := h.DB.RetryProductionProxyJob(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "media preparation job is not failed", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go h.dispatchProductionProxyQueue(context.Background())
	http.Redirect(w, r, "/worker?requeued_proxy="+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (h *Handler) logicalMediaSourcesRecursive(ctx context.Context, prefix string) ([]mediaFile, error) {
	lines, err := h.rcloneLsf(ctx, prefix, "--recursive", "--files-only")
	if err != nil {
		return nil, err
	}
	byDirectory := make(map[string][]string)
	for _, line := range lines {
		relative := strings.Trim(strings.TrimSpace(line), "/")
		if relative == "" || !isVideoFile(relative) || isProductionWorkspacePath(prefix+relative) {
			continue
		}
		directory, name := path.Split(relative)
		byDirectory[directory] = append(byDirectory[directory], name)
	}
	var sources []mediaFile
	for directory, names := range byDirectory {
		for _, source := range groupMediaFiles(prefix+directory, names) {
			if source.SourceType != "" {
				sources = append(sources, source)
			}
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
}

func isProductionWorkspacePath(objectKey string) bool {
	return strings.Contains(objectKey, "/recordings/workspace/")
}

func productionProxyObjectKey(conference string, source mediaFile) string {
	recordingsPrefix := strings.TrimSuffix(conference, "/") + "/recordings/"
	relative := strings.TrimPrefix(source.Path, recordingsPrefix)
	if relative == source.Path || relative == "" {
		return ""
	}
	directory, filename := path.Split(relative)
	extension := path.Ext(filename)
	stem := strings.TrimSuffix(filename, extension)
	if source.SourceType == "chunkedVideo" {
		if match := chunkSuffix.FindStringSubmatch(filename); len(match) == 4 {
			stem = strings.TrimRight(match[1], " ._-")
		}
	}
	if stem == "" {
		return ""
	}
	return recordingsPrefix + productionProxyDirectory + "/" + directory + stem + ".mp4"
}

func productionConferenceFromRecording(objectKey string) string {
	conference, _, ok := strings.Cut(strings.Trim(objectKey, "/"), "/recordings/")
	if !ok || !validProductionConference(conference) {
		return ""
	}
	return conference
}

func (h *Handler) productionProxyArtifactInventory(ctx context.Context, conference string) (map[string]bool, error) {
	proxies := make(map[string]bool)
	if strings.TrimSpace(h.Remote) == "" || !validProductionConference(conference) {
		return proxies, nil
	}
	prefix := conference + "/recordings/" + productionProxyDirectory + "/"
	lines, err := h.rcloneLsf(ctx, prefix, "--recursive", "--files-only")
	if err != nil {
		return nil, fmt.Errorf("inspect prepared media: %w", err)
	}
	for _, line := range lines {
		relative := strings.Trim(strings.TrimSpace(line), "/")
		if relative != "" && isVideoFile(relative) {
			proxies[prefix+relative] = true
		}
	}
	return proxies, nil
}

func (h *Handler) productionProxyArtifactCount(ctx context.Context, conference string) (int, error) {
	proxies, err := h.productionProxyArtifactInventory(ctx, conference)
	if err != nil {
		return 0, err
	}
	return len(proxies), nil
}

func (h *Handler) productionProxyArtifactsForSources(ctx context.Context, sources []mediaFile) (map[string]bool, error) {
	wanted := make(map[string]bool)
	directories := make(map[string]bool)
	for _, source := range sources {
		conference := productionConferenceFromRecording(source.Path)
		proxy := productionProxyObjectKey(conference, source)
		if proxy == "" {
			continue
		}
		wanted[proxy] = true
		directories[path.Dir(proxy)+"/"] = true
	}
	found := make(map[string]bool)
	if strings.TrimSpace(h.Remote) == "" {
		return found, nil
	}
	for directory := range directories {
		lines, err := h.rcloneLsf(ctx, directory, "--files-only")
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			objectKey := directory + strings.Trim(strings.TrimSpace(line), "/")
			if wanted[objectKey] {
				found[objectKey] = true
			}
		}
	}
	return found, nil
}

func (h *Handler) productionProxyArtifactPresent(ctx context.Context, proxy string) (present, checked bool) {
	if proxy == "" || strings.TrimSpace(h.Remote) == "" {
		return false, false
	}
	files, err := h.rcloneLsf(ctx, path.Dir(proxy)+"/", "--files-only")
	if err != nil {
		return false, false
	}
	name := path.Base(proxy)
	for _, file := range files {
		if strings.Trim(strings.TrimSpace(file), "/") == name {
			return true, true
		}
	}
	return false, true
}

func (h *Handler) productionProxyDispatcher() {
	if h.DB == nil || strings.TrimSpace(h.Remote) == "" {
		return
	}
	if err := h.DB.RequeueInterruptedProductionProxyJobs(); err != nil {
		log.Printf("requeueing interrupted production proxies failed: %v", err)
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		h.dispatchProductionProxyQueue(context.Background())
		<-ticker.C
	}
}

func (h *Handler) dispatchProductionProxyQueue(ctx context.Context) {
	if h.DB == nil || strings.TrimSpace(h.Remote) == "" {
		return
	}
	h.proxyQueueMu.Lock()
	defer h.proxyQueueMu.Unlock()
	for {
		job, err := h.DB.ClaimProductionProxyJob()
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			log.Printf("claiming production proxy job failed: %v", err)
			return
		}
		durationMS, err := h.prepareProductionProxy(ctx, job)
		if err != nil {
			log.Printf("prepare production proxy %s: %v", job.Source, err)
			if dbErr := h.DB.FailProductionProxyJob(job.ID, err); dbErr != nil {
				log.Printf("marking production proxy %d failed: %v", job.ID, dbErr)
			}
		} else if err := h.DB.FinishProductionProxyJob(job.ID, durationMS); err != nil {
			log.Printf("finishing production proxy %d failed: %v", job.ID, err)
		}
	}
}

func (h *Handler) prepareProductionProxy(parent context.Context, job db.ProductionProxyJob) (int64, error) {
	ctx, cancel := context.WithTimeout(parent, 48*time.Hour)
	defer cancel()
	source, err := h.logicalMediaSource(ctx, job.Source)
	if err != nil {
		return 0, fmt.Errorf("locate source sequence: %w", err)
	}
	chunks := source.Chunks
	if len(chunks) == 0 {
		chunks = []string{source.Path}
	}
	cacheDir := strings.TrimSpace(h.CacheDir)
	if cacheDir == "" {
		cacheDir = os.TempDir()
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return 0, fmt.Errorf("create proxy cache: %w", err)
	}
	workDir, err := os.MkdirTemp(cacheDir, "production-proxy-")
	if err != nil {
		return 0, fmt.Errorf("create proxy workspace: %w", err)
	}
	defer os.RemoveAll(workDir)

	var concat bytes.Buffer
	var sourceDurationMS int64
	for i, objectKey := range chunks {
		extension := strings.ToLower(path.Ext(objectKey))
		if extension == "" {
			extension = ".mp4"
		}
		local := filepath.Join(workDir, fmt.Sprintf("input-%05d%s", i, extension))
		stage := fmt.Sprintf("Downloading chunk %d of %d", i+1, len(chunks))
		h.updateProductionProxyProgress(job.ID, stage, 0)
		if err := h.runProxyRclone(ctx, job.ID, stage, "copyto", "--no-traverse", h.remotePath(objectKey), local); err != nil {
			return 0, fmt.Errorf("download %s: %w", objectKey, err)
		}
		if chunkDurationMS, err := proxyDurationMS(ctx, local); err == nil {
			sourceDurationMS += chunkDurationMS
		}
		fmt.Fprintf(&concat, "file '%s'\n", filepath.ToSlash(local))
	}
	concatPath := filepath.Join(workDir, "inputs.txt")
	if err := os.WriteFile(concatPath, concat.Bytes(), 0o600); err != nil {
		return 0, err
	}
	output := filepath.Join(workDir, "proxy.mp4")
	h.updateProductionProxyProgress(job.ID, "Encoding proxy", 0)
	if err := h.runProxyFFmpeg(ctx, job.ID, sourceDurationMS,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-fflags", "+genpts",
		"-f", "concat", "-safe", "0", "-i", concatPath,
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
		"-vf", "scale=-2:540",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "96k", "-ac", "2",
		"-movflags", "+faststart", "-avoid_negative_ts", "make_zero", output,
	); err != nil {
		return 0, fmt.Errorf("encode editing proxy: %w", err)
	}
	durationMS, err := proxyDurationMS(ctx, output)
	if err != nil {
		return 0, err
	}
	h.updateProductionProxyProgress(job.ID, "Uploading proxy", 0)
	if err := h.runProxyRclone(ctx, job.ID, "Uploading proxy", "copyto", "--no-traverse", output, h.remotePath(job.Proxy)); err != nil {
		return 0, fmt.Errorf("upload %s: %w", job.Proxy, err)
	}
	return durationMS, nil
}

func (h *Handler) updateProductionProxyProgress(id int64, stage string, percent int) {
	if err := h.DB.UpdateProductionProxyJobProgress(id, stage, percent); err != nil {
		log.Printf("updating production proxy %d progress failed: %v", id, err)
	}
}

func (h *Handler) runProxyFFmpeg(ctx context.Context, jobID, durationMS int64, args ...string) error {
	if len(args) == 0 {
		return errors.New("ffmpeg output path is required")
	}
	output := args[len(args)-1]
	args = append(args[:len(args)-1], "-progress", "pipe:1", "-nostats", output)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	lastPercent := -1
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok || key != "out_time_us" || durationMS <= 0 {
			continue
		}
		microseconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		percent := int(microseconds / 1000 * 100 / durationMS)
		if percent > 99 {
			percent = 99
		}
		if percent != lastPercent {
			h.updateProductionProxyProgress(jobID, "Encoding proxy", percent)
			lastPercent = percent
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("%s", commandError(stderr.Bytes(), waitErr))
	}
	return scanErr
}

func proxyDurationMS(ctx context.Context, filename string) (int64, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", filename)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("verify proxy: %s", commandError(out, err))
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("verify proxy: invalid duration %q", strings.TrimSpace(string(out)))
	}
	return int64(math.Round(seconds * 1000)), nil
}

func (h *Handler) runProxyRclone(ctx context.Context, jobID int64, stage string, args ...string) error {
	args = append([]string{"--stats", "1s", "--stats-one-line", "--stats-log-level", "NOTICE"}, args...)
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Env = rcloneEnv(h.RcloneConfig)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(pipe)
	scanner.Split(splitCRLF)
	lastPercent := -1
	for scanner.Scan() {
		line := scanner.Text()
		stderr.WriteString(line)
		stderr.WriteByte('\n')
		if percent, ok := transferPercent(line); ok && percent != lastPercent {
			h.updateProductionProxyProgress(jobID, stage, percent)
			lastPercent = percent
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("%s", commandError(append(stdout.Bytes(), stderr.Bytes()...), waitErr))
	}
	return scanErr
}

func splitCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\r' || b == '\n' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func transferPercent(line string) (int, bool) {
	percentAt := strings.IndexByte(line, '%')
	if percentAt < 1 {
		return 0, false
	}
	start := percentAt - 1
	for start >= 0 && line[start] >= '0' && line[start] <= '9' {
		start--
	}
	percent, err := strconv.Atoi(line[start+1 : percentAt])
	return percent, err == nil && percent >= 0 && percent <= 100
}

func commandError(output []byte, err error) string {
	detail := strings.TrimSpace(string(output))
	if len(detail) > 4000 {
		detail = detail[len(detail)-4000:]
	}
	if detail != "" {
		return detail
	}
	return err.Error()
}
