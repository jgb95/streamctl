package handlers

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
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
	"streamctl/internal/probe"
	"streamctl/internal/systemd"
)

//go:embed templates/*.html
var templateFS embed.FS

type Handler struct {
	DB           *db.DB
	Secret       string
	VideoDir     string
	CacheDir     string
	Remote       string
	RcloneConfig string
	Systemd      *systemd.Manager

	funcs template.FuncMap
}

func (h *Handler) Register(mux *http.ServeMux) {
	h.funcs = template.FuncMap{
		"formatTime": func(t time.Time) string { return t.Format("2006-01-02 15:04") },
		"contains": func(slice []int64, id int64) bool {
			for _, x := range slice {
				if x == id {
					return true
				}
			}
			return false
		},
	}

	mux.HandleFunc("/login", h.login)
	mux.HandleFunc("/logout", h.logout)

	mux.Handle("/", h.auth(http.HandlerFunc(h.index)))
	mux.Handle("/streams/new", h.auth(http.HandlerFunc(h.streamNew)))
	mux.Handle("/streams/create", h.auth(http.HandlerFunc(h.streamCreate)))
	mux.Handle("/streams/edit/", h.auth(http.HandlerFunc(h.streamEdit)))
	mux.Handle("/streams/update/", h.auth(http.HandlerFunc(h.streamUpdate)))
	mux.Handle("/streams/delete/", h.auth(http.HandlerFunc(h.streamDelete)))
	mux.Handle("/streams/start/", h.auth(http.HandlerFunc(h.streamStart)))
	mux.Handle("/streams/stop/", h.auth(http.HandlerFunc(h.streamStop)))
	mux.Handle("/streams/logs/", h.auth(http.HandlerFunc(h.streamLogs)))
	mux.Handle("/spaces/browse", h.auth(http.HandlerFunc(h.spacesBrowse)))

	mux.Handle("/endpoints", h.auth(http.HandlerFunc(h.endpoints)))
	mux.Handle("/endpoints/create", h.auth(http.HandlerFunc(h.endpointCreate)))
	mux.Handle("/endpoints/update/", h.auth(http.HandlerFunc(h.endpointUpdate)))
	mux.Handle("/endpoints/delete/", h.auth(http.HandlerFunc(h.endpointDelete)))
	mux.Handle("/endpoints/test/", h.auth(http.HandlerFunc(h.endpointTest)))
}

// ---------- auth ----------

const cookieName = "streamctl_session"

func (h *Handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(h.Secret)) != 1 {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.render(w, "login.html", nil)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.FormValue("secret")), []byte(h.Secret)) != 1 {
		h.render(w, "login.html", map[string]any{"Error": "incorrect secret"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    h.Secret,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 30,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:   cookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- streams ----------

type streamView struct {
	db.Stream
	Status      string
	NextTrigger string
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	streams, err := h.DB.ListStreams()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]streamView, len(streams))
	for i, s := range streams {
		views[i] = streamView{
			Stream:      s,
			Status:      h.Systemd.Status(s.ID),
			NextTrigger: h.Systemd.NextTrigger(s.ID),
		}
	}
	h.render(w, "streams.html", map[string]any{"Streams": views})
}

func (h *Handler) streamNew(w http.ResponseWriter, r *http.Request) {
	files, err := h.listVideos()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	endpoints, err := h.DB.ListEndpoints()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	allIDs := make([]int64, len(endpoints))
	for i, e := range endpoints {
		allIDs[i] = e.ID
	}
	h.render(w, "stream_form.html", map[string]any{
		"Videos":          files,
		"BitratesJSON":    bitratesJSON(h.VideoDir, files),
		"Endpoints":       endpoints,
		"SelectedIDs":     allIDs, // default = all
		"FormAction":      "/streams/create",
		"Title":           "New stream",
		"DefaultSchedule": "once",
		"RemoteBrowse":    strings.TrimSpace(h.Remote) != "",
	})
}

func (h *Handler) streamCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s, ids, videos, err := h.streamFromForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := h.DB.CreateStream(s, ids, videos)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	created, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.Systemd.Sync(created); err != nil {
		http.Error(w, "stream saved but systemd sync failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/edit/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	files, err := h.listVideos()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	endpoints, err := h.DB.ListEndpoints()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	selected := make([]int64, len(s.Endpoints))
	for i, e := range s.Endpoints {
		selected[i] = e.ID
	}
	h.render(w, "stream_form.html", map[string]any{
		"Stream":          s,
		"Videos":          files,
		"BitratesJSON":    bitratesJSON(h.VideoDir, files),
		"Endpoints":       endpoints,
		"SelectedIDs":     selected,
		"FormAction":      fmt.Sprintf("/streams/update/%d", s.ID),
		"Title":           "Edit stream",
		"DefaultSchedule": s.ScheduleType,
		"RemoteBrowse":    strings.TrimSpace(h.Remote) != "",
	})
}

func (h *Handler) streamUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/update/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s, ids, videos, err := h.streamFromForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.ID = id
	if err := h.DB.UpdateStream(s, ids, videos); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	updated, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.Systemd.Sync(updated); err != nil {
		http.Error(w, "stream saved but systemd sync failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/delete/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.Systemd.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.DB.DeleteStream(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamStart(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/start/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := h.Systemd.Start(s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamStop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/stop/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.Systemd.Stop(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/logs/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	h.render(w, "stream_logs.html", map[string]any{
		"Stream": s,
		"Units":  h.Systemd.Logs(id),
	})
}

type spacesEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type spacesBrowseResponse struct {
	Prefix string        `json:"prefix"`
	Dirs   []spacesEntry `json:"dirs"`
	Files  []spacesEntry `json:"files"`
}

func (h *Handler) spacesBrowse(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(h.Remote) == "" {
		http.Error(w, "remote browsing is not configured", http.StatusBadRequest)
		return
	}

	prefix, err := cleanSpacesPrefix(r.URL.Query().Get("prefix"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var resp spacesBrowseResponse
	resp.Prefix = prefix
	if prefix == "" {
		resp.Dirs, err = h.listSpacesConferences(r.Context())
	} else {
		resp.Dirs, resp.Files, err = h.listSpacesPrefix(r.Context(), prefix)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) streamFromForm(r *http.Request) (*db.Stream, []int64, []string, error) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return nil, nil, nil, fmt.Errorf("name required")
	}

	var videos []string
	for _, v := range r.Form["video_file"] {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		clean, err := cleanClipSource(v)
		if err != nil {
			return nil, nil, nil, err
		}
		videos = append(videos, clean)
	}
	if len(videos) == 0 {
		return nil, nil, nil, fmt.Errorf("at least one video clip required")
	}
	if err := h.validateCachedPlaylist(videos); err != nil {
		return nil, nil, nil, err
	}

	scheduleType := r.FormValue("schedule_type")
	if scheduleType != "once" && scheduleType != "recurring" {
		return nil, nil, nil, fmt.Errorf("schedule_type must be 'once' or 'recurring'")
	}
	onCalendar := strings.TrimSpace(r.FormValue("on_calendar"))
	if onCalendar == "" {
		return nil, nil, nil, fmt.Errorf("on_calendar required")
	}
	onCalendar = normalizeOnCalendar(onCalendar)

	var ids []int64
	for _, v := range r.Form["endpoint_ids"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bad endpoint id: %s", v)
		}
		ids = append(ids, id)
	}

	s := &db.Stream{
		Name:         name,
		ScheduleType: scheduleType,
		OnCalendar:   onCalendar,
		Enabled:      r.FormValue("enabled") == "on",
	}
	return s, ids, videos, nil
}

// ---------- endpoints ----------

func (h *Handler) endpoints(w http.ResponseWriter, r *http.Request) {
	eps, err := h.DB.ListEndpoints()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	testID, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	h.render(w, "endpoints.html", map[string]any{
		"Endpoints":  eps,
		"TestResult": q.Get("test"),
		"TestID":     testID,
		"TestErr":    q.Get("err"),
	})
}

func (h *Handler) endpointCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e := &db.Endpoint{
		Name:      strings.TrimSpace(r.FormValue("name")),
		RtmpURL:   strings.TrimSpace(r.FormValue("rtmp_url")),
		StreamKey: strings.TrimSpace(r.FormValue("stream_key")),
		Enabled:   true,
	}
	if e.Name == "" || e.RtmpURL == "" || e.StreamKey == "" {
		http.Error(w, "name, rtmp_url, stream_key required", http.StatusBadRequest)
		return
	}
	if _, err := h.DB.CreateEndpoint(e); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

func (h *Handler) endpointUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/endpoints/update/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	existing, err := h.DB.GetEndpoint(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	existing.Name = strings.TrimSpace(r.FormValue("name"))
	existing.RtmpURL = strings.TrimSpace(r.FormValue("rtmp_url"))
	if k := strings.TrimSpace(r.FormValue("stream_key")); k != "" {
		existing.StreamKey = k
	}
	existing.Enabled = r.FormValue("enabled") == "on"
	if err := h.DB.UpdateEndpoint(existing); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Resync any streams that use this endpoint.
	if err := h.resyncStreamsUsingEndpoint(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

// endpointTest pushes a short ffmpeg-generated test pattern to the endpoint
// to confirm the URL+key are accepted. Synchronous: blocks until ffmpeg
// exits or the timeout fires, then redirects back to /endpoints with the
// result encoded in the query string.
func (h *Handler) endpointTest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/endpoints/test/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	e, err := h.DB.GetEndpoint(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	target := strings.TrimRight(e.RtmpURL, "/") + "/" + e.StreamKey

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx,
		"ffmpeg", "-hide_banner", "-loglevel", "error",
		"-re",
		"-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-shortest", "-t", "8",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-pix_fmt", "yuv420p", "-b:v", "2000k",
		"-c:a", "aac", "-b:a", "128k", "-ar", "44100",
		"-f", "flv", target,
	)
	out, runErr := cmd.CombinedOutput()

	q := url.Values{}
	q.Set("id", strconv.FormatInt(id, 10))
	if runErr != nil {
		q.Set("test", "fail")
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = runErr.Error()
		}
		if len(msg) > 500 {
			msg = msg[:500] + "..."
		}
		q.Set("err", msg)
	} else {
		q.Set("test", "ok")
	}
	http.Redirect(w, r, "/endpoints?"+q.Encode(), http.StatusSeeOther)
}

func (h *Handler) endpointDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/endpoints/delete/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	streams, err := h.DB.ListStreams()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.DB.DeleteEndpoint(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Resync any stream that referenced the deleted endpoint.
	for _, s := range streams {
		for _, ep := range s.Endpoints {
			if ep.ID == id {
				updated, err := h.DB.GetStream(s.ID)
				if err == nil {
					_ = h.Systemd.Sync(updated)
				}
				break
			}
		}
	}
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

func (h *Handler) resyncStreamsUsingEndpoint(epID int64) error {
	streams, err := h.DB.ListStreams()
	if err != nil {
		return err
	}
	for _, s := range streams {
		for _, ep := range s.Endpoints {
			if ep.ID == epID {
				if err := h.Systemd.Sync(&s); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func (h *Handler) listSpacesConferences(ctx context.Context) ([]spacesEntry, error) {
	lines, err := h.rcloneLsf(ctx, "", "--dirs-only")
	if err != nil {
		return nil, err
	}
	var dirs []spacesEntry
	for _, line := range lines {
		name := strings.Trim(strings.TrimSpace(line), "/")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		dirs = append(dirs, spacesEntry{
			Name: name,
			Path: name + "/recordings/",
		})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	return dirs, nil
}

func (h *Handler) listSpacesPrefix(ctx context.Context, prefix string) ([]spacesEntry, []spacesEntry, error) {
	lines, err := h.rcloneLsf(ctx, prefix)
	if err != nil {
		return nil, nil, err
	}
	var dirs []spacesEntry
	var files []spacesEntry
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := strings.Trim(line, "/")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		if strings.HasSuffix(line, "/") {
			dirs = append(dirs, spacesEntry{Name: name, Path: prefix + name + "/"})
			continue
		}
		if isVideoFile(name) {
			files = append(files, spacesEntry{Name: name, Path: prefix + name})
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})
	return dirs, files, nil
}

func (h *Handler) rcloneLsf(ctx context.Context, prefix string, extraArgs ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{"lsf", h.remotePath(prefix)}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, "rclone", args...)
	if strings.TrimSpace(h.RcloneConfig) != "" {
		cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+h.RcloneConfig)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("rclone lsf %s: %s", prefix, msg)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

func (h *Handler) remotePath(prefix string) string {
	remote := strings.TrimRight(strings.TrimSpace(h.Remote), "/")
	if strings.HasSuffix(remote, ":") || prefix == "" {
		return remote + prefix
	}
	return remote + "/" + prefix
}

// ---------- helpers ----------

// bitratesJSON probes each file for its container bitrate and returns a JSON
// object {"file.mp4": 9876543, ...} suitable for embedding in a <script> tag.
// Files that ffprobe can't read or that don't expose a bitrate are omitted;
// the client falls back to 0 for those, which is fine for an estimate.
func bitratesJSON(videoDir string, files []string) template.JS {
	out := make(map[string]int, len(files))
	for _, f := range files {
		bps, err := probe.Bitrate(videoDir, f)
		if err != nil || bps == 0 {
			continue
		}
		out[f] = bps
	}
	b, err := json.Marshal(out)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

func (h *Handler) validateCachedPlaylist(videos []string) error {
	var paths []string
	var labels []string
	for _, v := range videos {
		path := h.localClipPath(v)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) && isRemoteClip(v) {
				continue
			}
			return fmt.Errorf("checking %s: %w", v, err)
		}
		paths = append(paths, path)
		labels = append(labels, v)
	}
	if len(paths) < 2 {
		return nil
	}
	return probe.ValidatePlaylistPaths(paths, labels)
}

func (h *Handler) localClipPath(source string) string {
	if isRemoteClip(source) {
		return filepath.Join(h.CacheDir, source)
	}
	return filepath.Join(h.VideoDir, source)
}

func isRemoteClip(source string) bool {
	return strings.Contains(source, "/")
}

func cleanSpacesPrefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(strings.ReplaceAll(prefix, "\\", "/"))
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	clean := path.Clean(prefix)
	if clean == "." {
		return "", nil
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("bad Spaces prefix: %q", prefix)
		}
	}
	if len(parts) < 2 || parts[1] != "recordings" {
		return "", fmt.Errorf("Spaces prefix must be inside <conference>/recordings")
	}
	return clean + "/", nil
}

func isVideoFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".mov", ".mkv", ".flv", ".ts":
		return true
	default:
		return false
	}
}

func cleanClipSource(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil
	}
	if filepath.IsAbs(source) {
		return "", fmt.Errorf("video_file must be relative: %q", source)
	}
	clean := filepath.ToSlash(filepath.Clean(source))
	if clean == "." || clean == "" {
		return "", nil
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("video_file must not contain traversal: %q", source)
		}
	}
	return clean, nil
}

func normalizeOnCalendar(onCalendar string) string {
	onCalendar = strings.TrimSpace(onCalendar)
	if len(onCalendar) > len("UTC") && strings.HasSuffix(onCalendar, "UTC") {
		i := len(onCalendar) - len("UTC") - 1
		if i >= 0 && onCalendar[i] != ' ' && onCalendar[i] != '\t' {
			return onCalendar[:i+1] + " UTC"
		}
	}
	return onCalendar
}

func (h *Handler) listVideos() ([]string, error) {
	entries, err := os.ReadDir(h.VideoDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		switch ext {
		case ".mp4", ".mov", ".mkv", ".flv", ".ts":
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	tmpls, err := fs.Sub(templateFS, "templates")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl, err := template.New("").Funcs(h.funcs).ParseFS(tmpls, "layout.html", name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func pathID(r *http.Request, prefix string) (int64, bool) {
	tail := strings.TrimPrefix(r.URL.Path, prefix)
	id, err := strconv.ParseInt(tail, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
