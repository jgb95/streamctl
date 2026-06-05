package handlers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"streamctl/internal/db"
	"streamctl/internal/doclient"
	"streamctl/internal/nostrpub"
	"streamctl/internal/probe"
	"streamctl/internal/systemd"
)

//go:embed templates/*.html
var templateFS embed.FS

type Handler struct {
	DB                 *db.DB
	Secret             string
	VideoDir           string
	CacheDir           string
	HLSDir             string
	Remote             string
	RcloneConfig       string
	NostrKeyDir        string
	NostrKeyOwner      string
	GPUWorkerHost      string
	GPUWorkerCommand   string
	DOTokenFile        string
	GPUDropletName     string
	GPUDropletRegion   string
	GPUDropletSize     string
	GPUDropletImage    string
	GPUSSHKeyName      string
	GPUWorkerUser      string
	GPUDestroyAfterJob bool
	Systemd            *systemd.Manager

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
	mux.Handle("/live/", http.StripPrefix("/live/", http.HandlerFunc(h.liveFile)))

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
	mux.Handle("/livestream-files", h.auth(http.HandlerFunc(h.livestreamFiles)))
	mux.Handle("/livestream-files/process", h.auth(http.HandlerFunc(h.livestreamFileProcess)))
	mux.Handle("/gpu-worker/create", h.auth(http.HandlerFunc(h.gpuWorkerCreate)))
	mux.Handle("/gpu-worker/destroy", h.auth(http.HandlerFunc(h.gpuWorkerDestroy)))
	mux.Handle("/gpu-worker/logs/", h.auth(http.HandlerFunc(h.gpuJobLogs)))

	mux.Handle("/endpoints", h.auth(http.HandlerFunc(h.endpoints)))
	mux.Handle("/endpoints/create", h.auth(http.HandlerFunc(h.endpointCreate)))
	mux.Handle("/endpoints/update/", h.auth(http.HandlerFunc(h.endpointUpdate)))
	mux.Handle("/endpoints/delete/", h.auth(http.HandlerFunc(h.endpointDelete)))
	mux.Handle("/endpoints/test/", h.auth(http.HandlerFunc(h.endpointTest)))

	mux.Handle("/nostr", h.auth(http.HandlerFunc(h.nostr)))
	mux.Handle("/nostr/keys/create", h.auth(http.HandlerFunc(h.nostrKeyCreate)))
	mux.Handle("/nostr/keys/update/", h.auth(http.HandlerFunc(h.nostrKeyUpdate)))
	mux.Handle("/nostr/keys/delete/", h.auth(http.HandlerFunc(h.nostrKeyDelete)))
	mux.Handle("/nostr/relays/create", h.auth(http.HandlerFunc(h.nostrRelayCreate)))
	mux.Handle("/nostr/relays/update/", h.auth(http.HandlerFunc(h.nostrRelayUpdate)))
	mux.Handle("/nostr/relays/delete/", h.auth(http.HandlerFunc(h.nostrRelayDelete)))
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

// liveFile serves generated HLS playlists and segments publicly. Nostr live
// activity events can point clients at these /live/... URLs.
func (h *Handler) liveFile(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean("/" + r.URL.Path)
	if clean == "/" || strings.Contains(clean, "/../") {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(clean, "/")
	full := filepath.Join(h.HLSDir, rel)
	if !strings.HasPrefix(full, filepath.Clean(h.HLSDir)+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}

	switch strings.ToLower(filepath.Ext(full)) {
	case ".m3u8":
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	case ".ts":
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "public, max-age=30")
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeFile(w, r, full)
}

// ---------- streams ----------

type streamView struct {
	db.Stream
	Status          string
	NextTrigger     string
	NextTriggerUnix int64
	LiveURL         string
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
			Stream:          s,
			Status:          h.Systemd.Status(s.ID),
			NextTrigger:     h.Systemd.NextTrigger(s.ID),
			NextTriggerUnix: h.Systemd.NextTriggerUnix(s.ID),
			LiveURL:         fmt.Sprintf("/live/stream-%d/index.m3u8", s.ID),
		}
	}
	h.render(w, "streams.html", map[string]any{"Streams": views})
}

func (h *Handler) streamNew(w http.ResponseWriter, r *http.Request) {
	if err := h.renderStreamForm(w, http.StatusOK, "New stream", "/streams/create", nil, nil, "", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *Handler) streamCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s, ids, videos, err := h.streamFromForm(r)
	if err != nil {
		draft := h.streamDraftFromForm(r)
		if renderErr := h.renderStreamForm(w, http.StatusBadRequest, "New stream", "/streams/create", draft, selectedEndpointIDsFromForm(r), draft.ScheduleType, err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	id, err := h.DB.CreateStream(s, ids, videos)
	if err != nil {
		s.Videos = videos
		if renderErr := h.renderStreamForm(w, http.StatusInternalServerError, "New stream", "/streams/create", s, ids, s.ScheduleType, err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	created, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.Systemd.Sync(created); err != nil {
		if renderErr := h.renderStreamForm(w, http.StatusInternalServerError, "Edit stream", fmt.Sprintf("/streams/update/%d", created.ID), created, endpointIDs(created.Endpoints), created.ScheduleType, "stream saved but systemd sync failed: "+err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
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
	selected := make([]int64, len(s.Endpoints))
	for i, e := range s.Endpoints {
		selected[i] = e.ID
	}
	if err := h.renderStreamForm(w, http.StatusOK, "Edit stream", fmt.Sprintf("/streams/update/%d", s.ID), s, selected, s.ScheduleType, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
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
		draft := h.streamDraftFromForm(r)
		draft.ID = id
		if renderErr := h.renderStreamForm(w, http.StatusBadRequest, "Edit stream", fmt.Sprintf("/streams/update/%d", id), draft, selectedEndpointIDsFromForm(r), draft.ScheduleType, err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	s.ID = id
	if err := h.DB.UpdateStream(s, ids, videos); err != nil {
		s.Videos = videos
		if renderErr := h.renderStreamForm(w, http.StatusInternalServerError, "Edit stream", fmt.Sprintf("/streams/update/%d", id), s, ids, s.ScheduleType, err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	updated, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.Systemd.Sync(updated); err != nil {
		if renderErr := h.renderStreamForm(w, http.StatusInternalServerError, "Edit stream", fmt.Sprintf("/streams/update/%d", id), updated, endpointIDs(updated.Endpoints), updated.ScheduleType, "stream saved but systemd sync failed: "+err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
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
	s, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := h.Systemd.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.Systemd.RemoveCachedClips(s); err != nil {
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

func (h *Handler) renderStreamForm(w http.ResponseWriter, status int, title, action string, s *db.Stream, selected []int64, defaultSchedule, errorMessage string) error {
	files, err := h.listVideos()
	if err != nil {
		return err
	}
	endpoints, err := h.DB.ListEndpoints()
	if err != nil {
		return err
	}
	nostrKeys, err := h.DB.ListNostrKeys()
	if err != nil {
		return err
	}
	if selected == nil {
		selected = make([]int64, len(endpoints))
		for i, e := range endpoints {
			selected[i] = e.ID
		}
	}
	if defaultSchedule == "" {
		defaultSchedule = "once"
		if s != nil && s.ScheduleType != "" {
			defaultSchedule = s.ScheduleType
		}
	}
	h.renderStatus(w, status, "stream_form.html", map[string]any{
		"Stream":          s,
		"Videos":          files,
		"BitratesJSON":    bitratesJSON(h.VideoDir, files),
		"Endpoints":       endpoints,
		"NostrKeys":       nostrKeys,
		"SelectedIDs":     selected,
		"FormAction":      action,
		"Title":           title,
		"DefaultSchedule": defaultSchedule,
		"RemoteBrowse":    strings.TrimSpace(h.Remote) != "",
		"Error":           errorMessage,
	})
	return nil
}

func (h *Handler) streamDraftFromForm(r *http.Request) *db.Stream {
	var videos []string
	for _, v := range r.Form["video_file"] {
		v = strings.TrimSpace(v)
		if v != "" {
			videos = append(videos, v)
		}
	}
	scheduleType := r.FormValue("schedule_type")
	if scheduleType == "" {
		scheduleType = "once"
	}
	return &db.Stream{
		Name:         strings.TrimSpace(r.FormValue("name")),
		Videos:       videos,
		ScheduleType: scheduleType,
		OnCalendar:   strings.TrimSpace(r.FormValue("on_calendar")),
		NostrEnabled: r.FormValue("nostr_enabled") == "on",
		NostrKeyID:   parseOptionalInt64(r.FormValue("nostr_key_id")),
		NostrTitle:   strings.TrimSpace(r.FormValue("nostr_title")),
		NostrSummary: strings.TrimSpace(r.FormValue("nostr_summary")),
		Enabled:      r.FormValue("enabled") == "on",
	}
}

func selectedEndpointIDsFromForm(r *http.Request) []int64 {
	var ids []int64
	for _, v := range r.Form["endpoint_ids"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func endpointIDs(endpoints []db.Endpoint) []int64 {
	ids := make([]int64, len(endpoints))
	for i, e := range endpoints {
		ids[i] = e.ID
	}
	return ids
}

func normalizeEndpointType(value string) string {
	switch strings.TrimSpace(value) {
	case "youtube_hls":
		return "youtube_hls"
	default:
		return "rtmp"
	}
}

func validateEndpoint(e *db.Endpoint, creating bool) error {
	if e.Name == "" {
		return fmt.Errorf("name required")
	}
	if e.RtmpURL == "" {
		if e.Type == "youtube_hls" {
			return fmt.Errorf("YouTube HLS URL required")
		}
		return fmt.Errorf("RTMP URL required")
	}
	switch e.Type {
	case "rtmp":
		if e.StreamKey == "" {
			return fmt.Errorf("stream key required")
		}
	case "youtube_hls":
		if !strings.Contains(e.RtmpURL, "file=") {
			return fmt.Errorf("YouTube HLS URL must include file=")
		}
	default:
		return fmt.Errorf("unsupported endpoint type: %s", e.Type)
	}
	return nil
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
		NostrEnabled: r.FormValue("nostr_enabled") == "on",
		NostrKeyID:   parseOptionalInt64(r.FormValue("nostr_key_id")),
		NostrTitle:   strings.TrimSpace(r.FormValue("nostr_title")),
		NostrSummary: strings.TrimSpace(r.FormValue("nostr_summary")),
		Enabled:      r.FormValue("enabled") == "on",
	}
	if s.NostrEnabled {
		if s.NostrKeyID == 0 {
			return nil, nil, nil, fmt.Errorf("Nostr key required when Nostr publishing is enabled")
		}
		if _, err := h.DB.GetNostrKey(s.NostrKeyID); err != nil {
			return nil, nil, nil, fmt.Errorf("Nostr key not found")
		}
	}
	return s, ids, videos, nil
}

func parseOptionalInt64(value string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return id
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
		Type:      normalizeEndpointType(r.FormValue("endpoint_type")),
		Name:      strings.TrimSpace(r.FormValue("name")),
		RtmpURL:   strings.TrimSpace(r.FormValue("rtmp_url")),
		StreamKey: strings.TrimSpace(r.FormValue("stream_key")),
		Enabled:   true,
	}
	if err := validateEndpoint(e, true); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	existing.Type = normalizeEndpointType(r.FormValue("endpoint_type"))
	existing.Name = strings.TrimSpace(r.FormValue("name"))
	existing.RtmpURL = strings.TrimSpace(r.FormValue("rtmp_url"))
	if k := strings.TrimSpace(r.FormValue("stream_key")); k != "" {
		existing.StreamKey = k
	}
	if existing.Type == "youtube_hls" {
		existing.StreamKey = ""
	}
	existing.Enabled = r.FormValue("enabled") == "on"
	if err := validateEndpoint(existing, false); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
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
	if e.Type != "rtmp" {
		http.Error(w, "test broadcast is only supported for RTMP endpoints", http.StatusBadRequest)
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

// ---------- Livestream files ----------

type livestreamFileView struct {
	Conference     string
	Name           string
	RawPath        string
	NormalizedPath string
	Processed      bool
}

type gpuWorkerView struct {
	Managed    bool
	Configured bool
	ID         int64
	Name       string
	Status     string
	IP         string
	SSHHost    string
	Error      string
}

type gpuJobView struct {
	UnitName    string
	RawPath     string
	Host        string
	Description string
	LoadedState string
	ActiveState string
	SubState    string
	Result      string
	Since       string
	Journal     string
	Error       string
}

type gpuStatusView struct {
	Host      string
	Available bool
	NvidiaSMI string
	Jobs      []gpuJobView
	Error     string
}

func (h *Handler) livestreamFiles(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(h.Remote) == "" {
		http.Error(w, "remote browsing is not configured", http.StatusBadRequest)
		return
	}
	files, err := h.listLivestreamFiles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	worker := h.gpuWorkerView(r.Context())
	gpuStatus := h.gpuStatus(r.Context(), worker)
	h.render(w, "livestream_files.html", map[string]any{
		"Files":         files,
		"GPUConfigured": worker.Configured,
		"GPUWorkerHost": h.GPUWorkerHost,
		"GPUWorker":     worker,
		"GPUStatus":     gpuStatus,
		"Started":       r.URL.Query().Get("started"),
		"Job":           r.URL.Query().Get("job"),
		"Error":         r.URL.Query().Get("err"),
	})
}

func (h *Handler) livestreamFileProcess(w http.ResponseWriter, r *http.Request) {
	if !h.gpuWorkerConfigured() {
		http.Error(w, "GPU worker is not configured", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rawPath := strings.TrimSpace(r.FormValue("path"))
	if _, err := livestreamNormalizedPath(rawPath); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	host, err := h.gpuWorkerSSHHost(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	command := strings.TrimSpace(h.GPUWorkerCommand)
	unitName, out, err := startRemoteGPUTranscode(r.Context(), host, command, rawPath)
	if err != nil {
		log.Printf("GPU transcode submit %s failed: %v: %s", rawPath, err, strings.TrimSpace(out))
		http.Error(w, fmt.Sprintf("starting GPU job failed: %v\n%s", err, strings.TrimSpace(out)), http.StatusBadGateway)
		return
	}
	log.Printf("GPU transcode %s submitted as %s: %s", rawPath, unitName, strings.TrimSpace(out))
	if err := h.saveGPUJobLog(gpuJobView{
		UnitName:    unitName,
		RawPath:     rawPath,
		Host:        host,
		Description: "streamctl GPU transcode " + rawPath,
		ActiveState: "queued",
	}); err != nil {
		log.Printf("saving GPU job %s failed: %v", unitName, err)
	}
	go h.monitorGPUJob(unitName, rawPath, host)

	q := url.Values{}
	q.Set("started", rawPath)
	q.Set("job", unitName)
	http.Redirect(w, r, "/livestream-files?"+q.Encode(), http.StatusSeeOther)
}

func (h *Handler) gpuJobLogs(w http.ResponseWriter, r *http.Request) {
	unit := strings.TrimPrefix(r.URL.Path, "/gpu-worker/logs/")
	if !validGPUUnitName(unit) {
		http.NotFound(w, r)
		return
	}
	host, err := h.gpuWorkerSSHHost(r.Context())
	if err != nil {
		if cached, dbErr := h.DB.GetGPUJobLog(unit); dbErr == nil {
			h.render(w, "gpu_job_logs.html", map[string]any{
				"Job":  gpuJobViewFromDB(*cached),
				"Host": cached.Host,
			})
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	job := h.gpuJob(ctx, host, unit, true)
	if strings.TrimSpace(job.Journal) == "" {
		if cached, err := h.DB.GetGPUJobLog(unit); err == nil {
			job = gpuJobViewFromDB(*cached)
		}
	}
	h.render(w, "gpu_job_logs.html", map[string]any{
		"Job":  job,
		"Host": host,
	})
}

func startRemoteGPUTranscode(ctx context.Context, host, command, rawPath string) (string, string, error) {
	unitName := gpuTranscodeUnitName(rawPath)
	remote := strings.Join([]string{
		"systemd-run",
		"--unit=" + shellQuote(unitName),
		"--description=" + shellQuote("streamctl GPU transcode "+rawPath),
		"--collect",
		"--property=" + shellQuote("Type=exec"),
		"--property=" + shellQuote("WorkingDirectory=/root"),
		"--property=" + shellQuote("Environment=RCLONE_CONFIG=/root/rclone.conf"),
		"--",
		shellQuote(command),
		shellQuote(rawPath),
	}, " ")
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", host, remote)
	out, err := cmd.CombinedOutput()
	return unitName, string(out), err
}

func (h *Handler) gpuStatus(ctx context.Context, worker gpuWorkerView) gpuStatusView {
	status := gpuStatusView{}
	if !worker.Configured {
		return status
	}
	host := strings.TrimSpace(h.GPUWorkerHost)
	if host == "" {
		if worker.Status != "active" || worker.SSHHost == "" {
			return h.appendCachedGPUJobs(status)
		}
		host = worker.SSHHost
	}
	status.Host = host
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	nvidia, err := remoteSSH(ctx, host, "nvidia-smi --query-gpu=name,utilization.gpu,utilization.memory,memory.used,memory.total,power.draw --format=csv,noheader,nounits 2>/dev/null || true")
	if err != nil {
		status.Error = fmt.Sprintf("GPU status unavailable: %v: %s", err, strings.TrimSpace(nvidia))
		return h.appendCachedGPUJobs(status)
	}
	status.Available = true
	status.NvidiaSMI = strings.TrimSpace(nvidia)
	unitsOut, err := remoteSSH(ctx, host, "systemctl list-units 'streamctl-gpu-*' --all --no-legend --plain 2>/dev/null || true")
	if err != nil {
		status.Error = fmt.Sprintf("GPU jobs unavailable: %v: %s", err, strings.TrimSpace(unitsOut))
		return h.appendCachedGPUJobs(status)
	}
	for _, unit := range parseGPUUnitList(unitsOut) {
		job := h.gpuJob(ctx, host, unit, false)
		status.Jobs = append(status.Jobs, job)
		if isTerminalGPUJob(job) {
			full := h.gpuJob(ctx, host, unit, true)
			if err := h.saveGPUJobLog(full); err != nil {
				log.Printf("saving terminal GPU job %s failed: %v", unit, err)
			}
			h.destroyManagedGPUAfterTerminalJob(ctx, full)
		}
		if len(status.Jobs) >= 10 {
			break
		}
	}
	return h.appendCachedGPUJobs(status)
}

func (h *Handler) appendCachedGPUJobs(status gpuStatusView) gpuStatusView {
	cached, err := h.DB.ListGPUJobLogs(10)
	if err != nil {
		if status.Error == "" {
			status.Error = fmt.Sprintf("cached GPU jobs unavailable: %v", err)
		}
		return status
	}
	seen := map[string]bool{}
	for _, job := range status.Jobs {
		seen[job.UnitName] = true
	}
	for _, cachedJob := range cached {
		if seen[cachedJob.UnitName] {
			continue
		}
		status.Jobs = append(status.Jobs, gpuJobViewFromDB(cachedJob))
		seen[cachedJob.UnitName] = true
		if len(status.Jobs) >= 10 {
			break
		}
	}
	return status
}

func (h *Handler) gpuJob(ctx context.Context, host, unit string, includeJournal bool) gpuJobView {
	job := gpuJobView{UnitName: unit, Host: host}
	show, err := remoteSSH(ctx, host, "systemctl show "+shellQuote(unit)+" --property=Description --property=LoadedState --property=ActiveState --property=SubState --property=Result --property=ActiveEnterTimestamp")
	if err != nil {
		job.Error = appendGPUError(job.Error, fmt.Errorf("systemctl show: %w: %s", err, strings.TrimSpace(show)))
	} else {
		populateGPUJobState(&job, show)
	}
	if includeJournal {
		journal, err := remoteSSH(ctx, host, "journalctl -u "+shellQuote(unit)+" --no-pager --output=short-iso -n 300")
		job.Journal = strings.TrimSpace(journal)
		if err != nil {
			job.Error = appendGPUError(job.Error, fmt.Errorf("journalctl: %w: %s", err, strings.TrimSpace(journal)))
		}
	}
	return job
}

func (h *Handler) monitorGPUJob(unitName, rawPath, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), 48*time.Hour)
	defer cancel()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("GPU job monitor %s stopped: %v", unitName, ctx.Err())
			return
		case <-ticker.C:
			job := h.gpuJob(ctx, host, unitName, false)
			job.RawPath = rawPath
			if err := h.saveGPUJobLog(job); err != nil {
				log.Printf("saving GPU job %s status failed: %v", unitName, err)
			}
			if !isTerminalGPUJob(job) {
				continue
			}
			full := h.gpuJob(ctx, host, unitName, true)
			full.RawPath = rawPath
			if err := h.saveGPUJobLog(full); err != nil {
				log.Printf("saving GPU job %s journal failed: %v", unitName, err)
			}
			h.destroyManagedGPUAfterTerminalJob(ctx, full)
			return
		}
	}
}

func (h *Handler) saveGPUJobLog(job gpuJobView) error {
	if strings.TrimSpace(job.UnitName) == "" {
		return nil
	}
	return h.DB.UpsertGPUJobLog(db.GPUJobLog{
		UnitName:    job.UnitName,
		RawPath:     job.RawPath,
		Host:        job.Host,
		Description: job.Description,
		ActiveState: job.ActiveState,
		SubState:    job.SubState,
		Result:      job.Result,
		Journal:     job.Journal,
		Error:       job.Error,
	})
}

func gpuJobViewFromDB(job db.GPUJobLog) gpuJobView {
	return gpuJobView{
		UnitName:    job.UnitName,
		RawPath:     job.RawPath,
		Host:        job.Host,
		Description: job.Description,
		ActiveState: job.ActiveState,
		SubState:    job.SubState,
		Result:      job.Result,
		Journal:     job.Journal,
		Error:       job.Error,
		Since:       job.UpdatedAt.Format(time.RFC3339),
	}
}

func isTerminalGPUJob(job gpuJobView) bool {
	return job.ActiveState == "inactive" || job.ActiveState == "failed"
}

func (h *Handler) destroyManagedGPUAfterTerminalJob(ctx context.Context, job gpuJobView) {
	if strings.TrimSpace(h.DOTokenFile) == "" {
		return
	}
	failed := job.ActiveState == "failed" || (job.Result != "" && job.Result != "success")
	shouldDestroy := failed || h.GPUDestroyAfterJob
	if !shouldDestroy {
		return
	}
	client, err := h.doClient()
	if err != nil {
		log.Printf("GPU worker cleanup skipped for %s: %v", job.UnitName, err)
		return
	}
	droplets, err := client.ListDropletsByTag(ctx, h.gpuWorkerTag())
	if err != nil {
		log.Printf("listing GPU workers for cleanup failed: %v", err)
		return
	}
	for _, d := range droplets {
		log.Printf("destroying GPU worker %d after job %s active=%s result=%s", d.ID, job.UnitName, job.ActiveState, job.Result)
		if err := client.DeleteDroplet(ctx, d.ID); err != nil {
			log.Printf("destroying GPU worker %d failed: %v", d.ID, err)
		}
	}
}

func remoteSSH(ctx context.Context, host, remoteCommand string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=5", host, remoteCommand)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func parseGPUUnitList(out string) []string {
	var units []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		unit := fields[0]
		if validGPUUnitName(unit) && !seen[unit] {
			units = append(units, unit)
			seen[unit] = true
		}
	}
	return units
}

func populateGPUJobState(job *gpuJobView, out string) {
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "Description":
			job.Description = val
		case "LoadedState":
			job.LoadedState = val
		case "ActiveState":
			job.ActiveState = val
		case "SubState":
			job.SubState = val
		case "Result":
			job.Result = val
		case "ActiveEnterTimestamp":
			job.Since = val
		}
	}
}

func validGPUUnitName(unit string) bool {
	if !strings.HasPrefix(unit, "streamctl-gpu-") {
		return false
	}
	for _, r := range unit {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' {
			continue
		}
		return false
	}
	return len(unit) <= 128
}

func appendGPUError(existing string, err error) string {
	if err == nil {
		return existing
	}
	if existing == "" {
		return err.Error()
	}
	return existing + "; " + err.Error()
}

func gpuTranscodeUnitName(rawPath string) string {
	sum := sha256.Sum256([]byte(rawPath))
	hash := hex.EncodeToString(sum[:])[:12]
	base := filepath.Base(rawPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	var safe strings.Builder
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			safe.WriteRune(r)
		} else if safe.Len() == 0 || safe.String()[safe.Len()-1] != '-' {
			safe.WriteByte('-')
		}
		if safe.Len() >= 36 {
			break
		}
	}
	name := strings.Trim(safe.String(), "-")
	if name == "" {
		name = "job"
	}
	return "streamctl-gpu-" + name + "-" + hash + ".service"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (h *Handler) gpuWorkerConfigured() bool {
	if strings.TrimSpace(h.GPUWorkerHost) != "" && strings.TrimSpace(h.GPUWorkerCommand) != "" {
		return true
	}
	return strings.TrimSpace(h.DOTokenFile) != "" && strings.TrimSpace(h.GPUWorkerCommand) != ""
}

func (h *Handler) gpuWorkerSSHHost(ctx context.Context) (string, error) {
	if strings.TrimSpace(h.GPUWorkerHost) != "" {
		return strings.TrimSpace(h.GPUWorkerHost), nil
	}
	worker := h.gpuWorkerView(ctx)
	if worker.Error != "" {
		return "", fmt.Errorf("%s", worker.Error)
	}
	if worker.IP == "" || worker.Status != "active" {
		return "", fmt.Errorf("managed GPU worker is not active")
	}
	return worker.SSHHost, nil
}

func (h *Handler) gpuWorkerView(ctx context.Context) gpuWorkerView {
	view := gpuWorkerView{
		Managed:    strings.TrimSpace(h.DOTokenFile) != "",
		Configured: h.gpuWorkerConfigured(),
		SSHHost:    strings.TrimSpace(h.GPUWorkerHost),
	}
	if !view.Managed {
		return view
	}
	client, err := h.doClient()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	droplets, err := client.ListDropletsByTag(ctx, h.gpuWorkerTag())
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if len(droplets) == 0 {
		return view
	}
	d := droplets[0]
	view.ID = d.ID
	view.Name = d.Name
	view.Status = d.Status
	view.IP = d.PublicIPv4()
	if view.IP != "" {
		view.SSHHost = firstNonEmptyString(h.GPUWorkerUser, "root") + "@" + view.IP
	}
	return view
}

func (h *Handler) gpuWorkerCreate(w http.ResponseWriter, r *http.Request) {
	client, err := h.doClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	existing, err := client.ListDropletsByTag(r.Context(), h.gpuWorkerTag())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if len(existing) > 0 {
		http.Redirect(w, r, "/livestream-files", http.StatusSeeOther)
		return
	}
	key, err := h.gpuSSHKey(r.Context(), client)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	userData, err := h.gpuWorkerUserData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := client.CreateDroplet(r.Context(), doclient.CreateDropletRequest{
		Name:       h.gpuDropletName(),
		Region:     firstNonEmptyString(h.GPUDropletRegion, "nyc2"),
		Size:       firstNonEmptyString(h.GPUDropletSize, "gpu-h100x1-80gb"),
		Image:      firstNonEmptyString(h.GPUDropletImage, "ubuntu-24-04-x64"),
		SSHKeys:    []string{key.Fingerprint},
		Monitoring: true,
		Tags:       []string{h.gpuWorkerTag(), "streamctl"},
		UserData:   userData,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/livestream-files", http.StatusSeeOther)
}

func (h *Handler) gpuWorkerDestroy(w http.ResponseWriter, r *http.Request) {
	client, err := h.doClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	droplets, err := client.ListDropletsByTag(r.Context(), h.gpuWorkerTag())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for _, d := range droplets {
		if err := client.DeleteDroplet(r.Context(), d.ID); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	http.Redirect(w, r, "/livestream-files", http.StatusSeeOther)
}

func (h *Handler) doClient() (*doclient.Client, error) {
	token, err := h.doToken()
	if err != nil {
		return nil, err
	}
	return doclient.New(token), nil
}

func (h *Handler) doToken() (string, error) {
	if strings.TrimSpace(h.DOTokenFile) == "" {
		return "", fmt.Errorf("DigitalOcean token file is not configured")
	}
	data, err := os.ReadFile(h.DOTokenFile)
	if err != nil {
		return "", fmt.Errorf("reading DigitalOcean token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("DigitalOcean token file is empty")
	}
	return token, nil
}

func (h *Handler) gpuSSHKey(ctx context.Context, client *doclient.Client) (*doclient.SSHKey, error) {
	keys, err := client.ListSSHKeys(ctx)
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(h.GPUSSHKeyName)
	for _, key := range keys {
		if key.Name == want || key.Fingerprint == want || strconv.FormatInt(key.ID, 10) == want {
			return &key, nil
		}
	}
	return nil, fmt.Errorf("DigitalOcean SSH key %q not found", want)
}

func (h *Handler) gpuWorkerUserData() (string, error) {
	rcloneConfig, err := os.ReadFile(h.RcloneConfig)
	if err != nil {
		return "", fmt.Errorf("reading rclone config: %w", err)
	}
	script := gpuTranscodeScript()
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euxo pipefail
DROPLET_ID="$(curl -fsS http://169.254.169.254/metadata/v1/id || true)"
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y ffmpeg rclone curl
install -d -m 0700 /root
base64 -d >/root/rclone.conf <<'STREAMCTL_RCLONE'
%s
STREAMCTL_RCLONE
base64 -d >/root/transcode-nvenc.sh <<'STREAMCTL_SCRIPT'
%s
STREAMCTL_SCRIPT
chmod 0400 /root/rclone.conf
chmod 0755 /root/transcode-nvenc.sh
echo "$DROPLET_ID" >/root/droplet-id
echo 'streamctl GPU worker ready'
`, base64.StdEncoding.EncodeToString(rcloneConfig), base64.StdEncoding.EncodeToString([]byte(script))), nil
}

func (h *Handler) gpuWorkerTag() string {
	return "streamctl-gpu-worker"
}

func (h *Handler) gpuDropletName() string {
	return firstNonEmptyString(h.GPUDropletName, "streamctl-gpu-worker")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func gpuTranscodeScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "usage: $0 <conference>/recordings/edits/livestream/<file.mp4>" >&2
  exit 2
fi

raw_path="${1#/}"
remote="${SPACES_REMOTE:-spaces:btcpp}"
workdir="${WORKDIR:-/tmp/streamctl-transcode}"
video_bitrate="${VIDEO_BITRATE:-6800k}"
audio_bitrate="${AUDIO_BITRATE:-160k}"

case "$raw_path" in
  */recordings/edits/livestream/*) ;;
  *)
    echo "path must be <conference>/recordings/edits/livestream/<file>" >&2
    exit 2
    ;;
esac

conference="${raw_path%%/recordings/edits/livestream/*}"
filename="${raw_path##*/}"
normalized_path="${conference}/recordings/normalized/livestream/${filename}"

remote_path() {
  case "$remote" in
    *:|*/) printf '%s%s' "$remote" "$1" ;;
    *) printf '%s/%s' "$remote" "$1" ;;
  esac
}

export RCLONE_CONFIG="${RCLONE_CONFIG:-/root/rclone.conf}"
mkdir -p "$workdir"
raw_file="${workdir}/${filename}"
out_file="${workdir}/${filename%.mp4}.normalized.mp4"
ready_file="${workdir}/${filename}.ready.json"

echo "transcode: downloading ${raw_path}"
rclone copyto "$(remote_path "$raw_path")" "$raw_file"

echo "transcode: encoding ${raw_path} -> ${normalized_path}"
rm -f -- "$out_file"
ffmpeg -hide_banner -loglevel error -stats_period 30 -progress pipe:1 -y \
  -i "$raw_file" \
  -map 0:v:0 -map 0:a:0 -dn -sn \
  -c:v h264_nvenc -preset p4 -profile:v high -pix_fmt yuv420p \
  -r 30 -g 60 -keyint_min 60 -sc_threshold 0 \
  -b:v "$video_bitrate" -maxrate "$video_bitrate" -bufsize "${VIDEO_BUFSIZE:-13600k}" \
  -c:a aac -b:a "$audio_bitrate" -ar 48000 -ac 2 \
  -movflags +faststart \
  "$out_file"

echo "transcode: verifying ${out_file}"
ffprobe -v error -show_streams "$out_file" >/dev/null

cat > "$ready_file" <<EOF
{
  "raw_path": "${raw_path}",
  "normalized_path": "${normalized_path}",
  "video_bitrate": "${video_bitrate}",
  "audio_bitrate": "${audio_bitrate}",
  "encoder": "h264_nvenc",
  "status": "ready"
}
EOF

echo "transcode: uploading ${normalized_path}"
rclone copyto "$out_file" "$(remote_path "$normalized_path")"
rclone copyto "$ready_file" "$(remote_path "${normalized_path}.ready.json")"

echo "transcode: ready ${normalized_path}"
`
}

func (h *Handler) listLivestreamFiles(ctx context.Context) ([]livestreamFileView, error) {
	confs, err := h.listSpacesConferences(ctx)
	if err != nil {
		return nil, err
	}
	var out []livestreamFileView
	for _, conf := range confs {
		name := strings.TrimSuffix(strings.TrimSuffix(conf.Path, "/recordings/"), "/")
		rawPrefix := name + "/recordings/edits/livestream/"
		rawFiles, err := h.listSpacesFilesOnly(ctx, rawPrefix)
		if err != nil {
			continue
		}
		normalizedPrefix := name + "/recordings/normalized/livestream/"
		normalizedFiles, err := h.listSpacesFilesOnly(ctx, normalizedPrefix)
		if err != nil {
			normalizedFiles = nil
		}
		processed := make(map[string]bool, len(normalizedFiles))
		for _, f := range normalizedFiles {
			processed[f.Name] = true
		}
		for _, f := range rawFiles {
			normalizedPath, err := livestreamNormalizedPath(f.Path)
			if err != nil {
				continue
			}
			out = append(out, livestreamFileView{
				Conference:     name,
				Name:           f.Name,
				RawPath:        f.Path,
				NormalizedPath: normalizedPath,
				Processed:      processed[f.Name],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if strings.ToLower(out[i].Conference) == strings.ToLower(out[j].Conference) {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return strings.ToLower(out[i].Conference) < strings.ToLower(out[j].Conference)
	})
	return out, nil
}

func (h *Handler) listSpacesFilesOnly(ctx context.Context, prefix string) ([]spacesEntry, error) {
	_, files, err := h.listSpacesPrefix(ctx, prefix)
	return files, err
}

func livestreamNormalizedPath(rawPath string) (string, error) {
	rawPath = strings.TrimSpace(strings.ReplaceAll(rawPath, "\\", "/"))
	parts := strings.Split(rawPath, "/")
	if len(parts) < 5 || parts[1] != "recordings" || parts[2] != "edits" || parts[3] != "livestream" {
		return "", fmt.Errorf("path must be <conference>/recordings/edits/livestream/<file>")
	}
	file := parts[len(parts)-1]
	if !isVideoFile(file) {
		return "", fmt.Errorf("path must be a video file")
	}
	return parts[0] + "/recordings/normalized/livestream/" + file, nil
}

// ---------- Nostr ----------

func (h *Handler) nostr(w http.ResponseWriter, r *http.Request) {
	keys, err := h.DB.ListNostrKeys()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	relays, err := h.DB.ListNostrRelays()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, "nostr.html", map[string]any{
		"Keys":   keys,
		"Relays": relays,
		"Error":  r.URL.Query().Get("err"),
	})
}

func (h *Handler) nostrKeyCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("key name required"), http.StatusSeeOther)
		return
	}
	info, err := nostrpub.DecodeKey(r.FormValue("nsec"))
	if err != nil {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("invalid nsec: "+err.Error()), http.StatusSeeOther)
		return
	}
	if err := os.MkdirAll(h.NostrKeyDir, 0750); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.CreateTemp(h.NostrKeyDir, "nostr-*.key")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	secretPath := f.Name()
	_ = f.Close()
	if err := nostrpub.WriteSecretFile(secretPath, info.Secret); err != nil {
		_ = os.Remove(secretPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.chownNostrKey(secretPath); err != nil {
		_ = os.Remove(secretPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = h.DB.CreateNostrKey(&db.NostrKey{
		Name:       name,
		PubKey:     info.PubKey,
		Npub:       info.Npub,
		SecretPath: secretPath,
		Enabled:    true,
	})
	if err != nil {
		_ = os.Remove(secretPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrKeyUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/nostr/keys/update/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key, err := h.DB.GetNostrKey(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	key.Name = strings.TrimSpace(r.FormValue("name"))
	key.Enabled = r.FormValue("enabled") == "on"
	if key.Name == "" {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("key name required"), http.StatusSeeOther)
		return
	}
	if err := h.DB.UpdateNostrKey(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrKeyDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/nostr/keys/delete/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	key, err := h.DB.GetNostrKey(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := h.DB.DeleteNostrKey(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Remove(key.SecretPath)
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrRelayCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	relay := &db.NostrRelay{
		URL:     normalizeRelayURL(r.FormValue("url")),
		Enabled: true,
	}
	if relay.URL == "" {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("relay URL required"), http.StatusSeeOther)
		return
	}
	if _, err := h.DB.CreateNostrRelay(relay); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrRelayUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/nostr/relays/update/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	relay := &db.NostrRelay{
		ID:      id,
		URL:     normalizeRelayURL(r.FormValue("url")),
		Enabled: r.FormValue("enabled") == "on",
	}
	if relay.URL == "" {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("relay URL required"), http.StatusSeeOther)
		return
	}
	if err := h.DB.UpdateNostrRelay(relay); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrRelayDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/nostr/relays/delete/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.DB.DeleteNostrRelay(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) chownNostrKey(path string) error {
	owner := strings.TrimSpace(h.NostrKeyOwner)
	if owner == "" {
		return nil
	}
	u, err := user.Lookup(owner)
	if err != nil {
		return nil
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil
	}
	return os.Chown(path, uid, gid)
}

func normalizeRelayURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "wss://") && !strings.HasPrefix(value, "ws://") {
		value = "wss://" + value
	}
	return value
}

func (h *Handler) resyncNostrStreams() error {
	streams, err := h.DB.ListStreams()
	if err != nil {
		return err
	}
	for _, s := range streams {
		if s.NostrEnabled {
			if err := h.Systemd.Sync(&s); err != nil {
				return err
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
	h.renderStatus(w, http.StatusOK, name, data)
}

func (h *Handler) renderStatus(w http.ResponseWriter, status int, name string, data any) {
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
	w.WriteHeader(status)
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
