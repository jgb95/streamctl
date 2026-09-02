package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
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

const productionProxyDirectory = "production/proxies"

func (h *Handler) productionProxyPrepare(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	prefix, err := cleanSpacesPrefix(r.FormValue("prefix"))
	if !validProductionConference(conference) || err != nil || !strings.HasPrefix(prefix, conference+"/recordings/") || isDerivedRecordingPath(prefix) {
		http.Error(w, "choose a source folder inside this conference's recordings workspace", http.StatusBadRequest)
		return
	}
	if h.DB == nil || strings.TrimSpace(h.Remote) == "" {
		http.Error(w, "media preparation is not configured", http.StatusServiceUnavailable)
		return
	}
	sources, err := h.logicalMediaSourcesRecursive(r.Context(), prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	queued := 0
	for _, source := range sources {
		proxy := productionProxyObjectKey(conference, source)
		if proxy == "" {
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
	go h.dispatchProductionProxyQueue(context.Background())
	destination := "/production/media?conference=" + url.QueryEscape(conference) + "&prefix=" + url.QueryEscape(prefix) + "&queued=" + strconv.Itoa(queued)
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

func (h *Handler) logicalMediaSourcesRecursive(ctx context.Context, prefix string) ([]mediaFile, error) {
	lines, err := h.rcloneLsf(ctx, prefix, "--recursive", "--files-only")
	if err != nil {
		return nil, err
	}
	byDirectory := make(map[string][]string)
	for _, line := range lines {
		relative := strings.Trim(strings.TrimSpace(line), "/")
		if relative == "" || !isVideoFile(relative) || isDerivedRecordingPath(prefix+relative) {
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

func isDerivedRecordingPath(objectKey string) bool {
	for _, marker := range []string{"/recordings/production/", "/recordings/edits/", "/recordings/normalized/", "/recordings/assets/"} {
		if strings.Contains(objectKey, marker) {
			return true
		}
	}
	return false
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
	for i, objectKey := range chunks {
		extension := strings.ToLower(path.Ext(objectKey))
		if extension == "" {
			extension = ".mp4"
		}
		local := filepath.Join(workDir, fmt.Sprintf("input-%05d%s", i, extension))
		if err := h.runProxyCommand(ctx, "rclone", "copyto", "--no-traverse", h.remotePath(objectKey), local); err != nil {
			return 0, fmt.Errorf("download %s: %w", objectKey, err)
		}
		fmt.Fprintf(&concat, "file '%s'\n", filepath.ToSlash(local))
	}
	concatPath := filepath.Join(workDir, "inputs.txt")
	if err := os.WriteFile(concatPath, concat.Bytes(), 0o600); err != nil {
		return 0, err
	}
	output := filepath.Join(workDir, "proxy.mp4")
	if err := h.runProxyCommand(ctx, "ffmpeg",
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
	if err := h.runProxyCommand(ctx, "rclone", "copyto", "--no-traverse", output, h.remotePath(job.Proxy)); err != nil {
		return 0, fmt.Errorf("upload %s: %w", job.Proxy, err)
	}
	return durationMS, nil
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

func (h *Handler) runProxyCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = rcloneEnv(h.RcloneConfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", commandError(out, err))
	}
	return nil
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
