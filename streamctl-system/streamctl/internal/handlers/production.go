package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

type productionConferenceView struct {
	btcppclient.Conference
	Date string
}

type productionNavView struct {
	Active      string
	Action      string
	Conference  string
	Conferences []productionConferenceView
}

type productionHomePage struct {
	Nav   productionNavView
	Error string
}

type productionTalkView struct {
	btcppclient.Candidate
	Cuts     []db.ProductionCut `json:"cuts"`
	CutCount int                `json:"-"`
	URL      string             `json:"-"`
}

type productionTalksPage struct {
	Nav        productionNavView
	Conference string
	Talks      []productionTalkView
	Error      string
}

type productionCutterJSON struct {
	Conference string             `json:"conference"`
	Talk       productionTalkView `json:"talk"`
	NextURL    string             `json:"nextUrl"`
}

type productionCutPage struct {
	Nav        productionNavView
	Conference string
	Talk       productionTalkView
	CutterJSON template.JS
	Previous   string
	Next       string
}

const productionConferenceCookie = "streamctl_production_conference"

func selectedProductionConference(r *http.Request) string {
	if conference := strings.TrimSpace(r.URL.Query().Get("conference")); conference != "" {
		return conference
	}
	cookie, err := r.Cookie(productionConferenceCookie)
	if err != nil || !validProductionConference(cookie.Value) {
		return ""
	}
	return cookie.Value
}

func rememberProductionConference(w http.ResponseWriter, r *http.Request, conference string) {
	if !validProductionConference(conference) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: productionConferenceCookie, Value: conference, Path: "/production",
		MaxAge: 365 * 24 * 60 * 60, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) productionHome(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	conferences, conferenceError := h.productionConferences(r.Context())
	h.render(w, r, "production.html", productionHomePage{
		Nav: productionNavView{
			Active: "overview", Action: "/production", Conference: conference, Conferences: conferences,
		},
		Error: conferenceError,
	})
}

func (h *Handler) productionTimestamp(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	page := productionTalksPage{Conference: conference}
	var conferenceError string
	page.Nav.Conferences, conferenceError = h.productionConferences(r.Context())
	page.Nav.Active = "timestamp"
	page.Nav.Action = "/production/timestamp"
	page.Nav.Conference = conference
	page.Error = conferenceError
	if conference == "" {
		h.render(w, r, "production_talks.html", page)
		return
	}
	if !validProductionConference(conference) {
		page.Error = "Invalid conference tag."
		h.renderStatus(w, r, http.StatusBadRequest, "production_talks.html", page)
		return
	}
	var err error
	page.Talks, err = h.productionTalks(r.Context(), conference)
	if err != nil {
		if page.Error == "" {
			page.Error = err.Error()
		}
	} else {
		cuts, err := h.DB.ListProductionCuts(conference)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for i := range page.Talks {
			page.Talks[i].Cuts = cuts[page.Talks[i].TalkID]
			page.Talks[i].CutCount = len(page.Talks[i].Cuts)
			page.Talks[i].URL = productionCutURL(conference, page.Talks[i].TalkID)
		}
	}
	h.render(w, r, "production_talks.html", page)
}

func (h *Handler) productionCut(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	talkID := strings.TrimSpace(r.URL.Query().Get("talk_id"))
	if !validProductionConference(conference) || talkID == "" {
		http.Error(w, "conference and talk_id are required", http.StatusBadRequest)
		return
	}
	talks, err := h.productionTalks(r.Context(), conference)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	index := -1
	for i := range talks {
		if talks[i].TalkID == talkID {
			index = i
			break
		}
	}
	if index < 0 {
		http.Error(w, "talk not found", http.StatusNotFound)
		return
	}
	cuts, err := h.DB.ListProductionCuts(conference)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	talk := talks[index]
	talk.Cuts = cuts[talk.TalkID]
	page := productionCutPage{Conference: conference, Talk: talk}
	page.Nav.Active = "timestamp"
	page.Nav.Action = "/production/timestamp"
	page.Nav.Conference = conference
	page.Nav.Conferences, _ = h.productionConferences(r.Context())
	if index > 0 {
		page.Previous = productionCutURL(conference, talks[index-1].TalkID)
	}
	if index+1 < len(talks) {
		page.Next = productionCutURL(conference, talks[index+1].TalkID)
	}
	encoded, err := json.Marshal(productionCutterJSON{Conference: conference, Talk: talk, NextURL: page.Next})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page.CutterJSON = template.JS(encoded)
	h.render(w, r, "production_cut.html", page)
}

func (h *Handler) productionConferences(ctx context.Context) ([]productionConferenceView, string) {
	if h.BTCPP == nil {
		return nil, "Bitcoin++ production API is not configured; set -btcpp-api-token-file."
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conferences, err := h.BTCPP.Conferences(ctx)
	if err != nil {
		return nil, "Loading Bitcoin++ conferences failed: " + err.Error()
	}
	sort.SliceStable(conferences, func(i, j int) bool { return conferenceSortKey(conferences[i]) > conferenceSortKey(conferences[j]) })
	views := make([]productionConferenceView, 0, len(conferences))
	for _, conference := range conferences {
		date := "Date not set"
		if conference.StartsAt != nil {
			if parsed, err := time.Parse(time.RFC3339, *conference.StartsAt); err == nil {
				date = parsed.Format("Jan 2, 2006")
			} else {
				date = *conference.StartsAt
			}
		}
		views = append(views, productionConferenceView{Conference: conference, Date: date})
	}
	return views, ""
}

func conferenceSortKey(conference btcppclient.Conference) string {
	if conference.StartsAt == nil {
		return ""
	}
	return *conference.StartsAt
}

func (h *Handler) productionTalks(ctx context.Context, conference string) ([]productionTalkView, error) {
	if h.BTCPP == nil {
		return nil, fmt.Errorf("Bitcoin++ production API is not configured; set -btcpp-api-token-file")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	candidates, err := h.BTCPP.RecordingCandidates(ctx, conference)
	if err != nil {
		return nil, fmt.Errorf("loading Bitcoin++ talks failed: %w", err)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidateSortKey(candidates[i]) < candidateSortKey(candidates[j]) })
	talks := make([]productionTalkView, len(candidates))
	for i, candidate := range candidates {
		talks[i].Candidate = candidate
	}
	return talks, nil
}

func candidateSortKey(candidate btcppclient.Candidate) string {
	start := "9999"
	if candidate.StartsAt != nil {
		start = *candidate.StartsAt
	}
	return start + "\x00" + candidate.Venue + "\x00" + candidate.Title
}

func productionCutURL(conference, talkID string) string {
	return "/production/timestamp/cut?conference=" + url.QueryEscape(conference) + "&talk_id=" + url.QueryEscape(talkID)
}

func validProductionConference(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			continue
		}
		return false
	}
	return true
}

type mediaWorkspacePage struct {
	Nav         productionNavView
	Conference  string
	Prefix      string
	Breadcrumbs []mediaBreadcrumb
	Dirs        []spacesEntry
	Files       []mediaFile
	Queued      string
	CanPrepare  bool
	Error       string
}

type mediaFile struct {
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	SourceType  string   `json:"sourceType,omitempty"`
	Chunks      []string `json:"chunks,omitempty"`
	ProxyStatus string   `json:"proxyStatus,omitempty"`
}

type mediaBrowseResponse struct {
	Prefix string        `json:"prefix"`
	Dirs   []spacesEntry `json:"dirs"`
	Files  []mediaFile   `json:"files"`
}

type logicalMediaInfo struct {
	Path        string `json:"path"`
	SourceType  string `json:"sourceType"`
	DurationMS  int64  `json:"durationMs"`
	ProxyPath   string `json:"proxyPath,omitempty"`
	ProxyStatus string `json:"proxyStatus,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

type mediaBreadcrumb struct {
	Name string
	URL  string
}

func (h *Handler) mediaWorkspace(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	page := mediaWorkspacePage{Conference: conference}
	page.Nav.Conferences, page.Error = h.productionConferences(r.Context())
	page.Nav.Active = "media"
	page.Nav.Action = "/production/media"
	page.Nav.Conference = conference
	if conference == "" {
		h.render(w, r, "media.html", page)
		return
	}
	if !validProductionConference(conference) {
		http.Error(w, "invalid conference", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(h.Remote) == "" {
		page.Error = "Spaces remote is not configured."
		h.render(w, r, "media.html", page)
		return
	}
	prefix := strings.TrimSpace(r.URL.Query().Get("prefix"))
	if prefix == "" {
		prefix = conference + "/recordings/"
	}
	clean, err := cleanSpacesPrefix(prefix)
	if err != nil || !strings.HasPrefix(clean, conference+"/recordings/") {
		http.Error(w, "media path must stay inside this conference's recordings folder", http.StatusBadRequest)
		return
	}
	page.Prefix = clean
	page.Queued = strings.TrimSpace(r.URL.Query().Get("queued"))
	page.CanPrepare = clean != conference+"/recordings/" && !isDerivedRecordingPath(clean)
	page.Breadcrumbs = mediaBreadcrumbs(conference, clean)
	page.Dirs, page.Files, err = h.listMediaSpacesPrefix(r.Context(), clean)
	if err != nil {
		page.Error = err.Error()
	}
	h.render(w, r, "media.html", page)
}

func (h *Handler) listMediaSpacesPrefix(ctx context.Context, prefix string) ([]spacesEntry, []mediaFile, error) {
	lines, err := h.rcloneLsf(ctx, prefix)
	if err != nil {
		return nil, nil, err
	}
	dirs := make([]spacesEntry, 0)
	names := make([]string, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		name := strings.Trim(line, "/")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		entry := spacesEntry{Name: name, Path: prefix + name}
		if strings.HasSuffix(line, "/") {
			entry.Path += "/"
			dirs = append(dirs, entry)
		} else {
			names = append(names, name)
		}
	}
	files := groupMediaFiles(prefix, names)
	if strings.Contains(prefix, "/recordings/production/") {
		for i := range files {
			files[i].SourceType = ""
		}
	}
	h.attachProductionProxyStatuses(files)
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	return dirs, files, nil
}

var chunkSuffix = regexp.MustCompile(`^(.*?)(\d+)(\.[^.]+)$`)

func groupMediaFiles(prefix string, names []string) []mediaFile {
	type chunk struct {
		name   string
		number int
	}
	groups := make(map[string][]chunk)
	for _, name := range names {
		match := chunkSuffix.FindStringSubmatch(name)
		if len(match) == 4 && len(match[2]) >= 2 && strings.HasPrefix(match[2], "0") {
			number, _ := strconv.Atoi(match[2])
			key := match[1] + "\x00" + strconv.Itoa(len(match[2])) + "\x00" + match[3]
			groups[key] = append(groups[key], chunk{name: name, number: number})
		}
	}
	consumed := make(map[string]bool)
	files := make([]mediaFile, 0)
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].number < group[j].number })
		consecutive := true
		for i := 1; i < len(group); i++ {
			if group[i].number != group[i-1].number+1 {
				consecutive = false
			}
		}
		if !consecutive {
			continue
		}
		chunks := make([]string, len(group))
		for i, item := range group {
			chunks[i] = prefix + item.name
			consumed[item.name] = true
		}
		files = append(files, mediaFile{Name: group[0].name + " … " + group[len(group)-1].name, Path: chunks[0], SourceType: "chunkedVideo", Chunks: chunks})
	}
	for _, name := range names {
		if consumed[name] {
			continue
		}
		sourceType := ""
		if isVideoFile(name) {
			sourceType = "video"
		}
		files = append(files, mediaFile{Name: name, Path: prefix + name, SourceType: sourceType})
	}
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name) })
	return files
}

func (h *Handler) mediaBrowse(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.URL.Query().Get("conference"))
	prefix := strings.TrimSpace(r.URL.Query().Get("prefix"))
	if prefix == "" {
		prefix = conference + "/recordings/"
	}
	clean, err := cleanSpacesPrefix(prefix)
	if !validProductionConference(conference) || err != nil || !strings.HasPrefix(clean, conference+"/recordings/") {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	dirs, files, err := h.listMediaSpacesPrefix(r.Context(), clean)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mediaBrowseResponse{Prefix: clean, Dirs: dirs, Files: files})
}

func (h *Handler) mediaInfo(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.URL.Query().Get("conference"))
	objectKey := strings.Trim(strings.TrimSpace(r.URL.Query().Get("path")), "/")
	if !validProductionConference(conference) || !strings.HasPrefix(objectKey, conference+"/recordings/") {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	clean, err := validateRenderObjectKey(objectKey)
	if err != nil || clean != objectKey {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	info, err := h.logicalMediaInfo(r.Context(), clean)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (h *Handler) logicalMediaInfo(ctx context.Context, objectKey string) (logicalMediaInfo, error) {
	selected, err := h.logicalMediaSource(ctx, objectKey)
	if err != nil {
		return logicalMediaInfo{}, err
	}
	info := h.attachProductionProxyInfo(logicalMediaInfo{Path: selected.Path, SourceType: selected.SourceType})
	if info.ProxyStatus == "" {
		info.Warning = "Prepare this recording from the Media page before cutting it."
	}
	return info, nil
}

func (h *Handler) attachProductionProxyStatuses(files []mediaFile) {
	if h.DB == nil {
		return
	}
	for i := range files {
		if files[i].SourceType == "" {
			continue
		}
		job, err := h.DB.ProductionProxyJobBySource(files[i].Path)
		if err != nil {
			continue
		}
		files[i].ProxyStatus = job.Status
	}
}

func (h *Handler) attachProductionProxyInfo(info logicalMediaInfo) logicalMediaInfo {
	if h.DB == nil {
		return info
	}
	job, err := h.DB.ProductionProxyJobBySource(info.Path)
	if err != nil {
		return info
	}
	info.ProxyStatus = job.Status
	if job.Status == "finished" {
		info.ProxyPath = job.Proxy
		info.DurationMS = job.DurationMS
		info.Warning = ""
	} else if job.Status == "failed" {
		info.Warning = "Proxy preparation failed: " + job.LastError
	} else {
		info.Warning = "Editing proxy is " + job.Status + "."
	}
	return info
}

func (h *Handler) logicalMediaSource(ctx context.Context, objectKey string) (mediaFile, error) {
	prefix := path.Dir(objectKey) + "/"
	_, files, err := h.listMediaSpacesPrefix(ctx, prefix)
	if err != nil {
		return mediaFile{}, err
	}
	for _, file := range files {
		if file.Path == objectKey && file.SourceType != "" {
			return file, nil
		}
	}
	return mediaFile{}, fmt.Errorf("media source was not found")
}

func mediaBreadcrumbs(conference, prefix string) []mediaBreadcrumb {
	crumbs := []mediaBreadcrumb{{Name: conference, URL: "/production/media?conference=" + url.QueryEscape(conference)}}
	relative := strings.Trim(strings.TrimPrefix(prefix, conference+"/recordings/"), "/")
	if relative == "" {
		return crumbs
	}
	current := conference + "/recordings/"
	for _, part := range strings.Split(relative, "/") {
		current += part + "/"
		crumbs = append(crumbs, mediaBreadcrumb{Name: part, URL: "/production/media?conference=" + url.QueryEscape(conference) + "&prefix=" + url.QueryEscape(current)})
	}
	return crumbs
}

func (h *Handler) mediaOpen(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.URL.Query().Get("conference"))
	objectKey := strings.Trim(strings.TrimSpace(r.URL.Query().Get("path")), "/")
	if !validProductionConference(conference) || !strings.HasPrefix(objectKey, conference+"/recordings/") {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	clean, err := validateRenderObjectKey(objectKey)
	if err != nil || clean != objectKey {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	link, err := h.productionObjectLink(r.Context(), clean)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, link, http.StatusTemporaryRedirect)
}

func (h *Handler) productionCutsSave(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	talkID := strings.TrimSpace(r.FormValue("talk_id"))
	sources, sourceTypes, inValues, outValues := r.Form["source"], r.Form["source_type"], r.Form["in_ms"], r.Form["out_ms"]
	if !validProductionConference(conference) || talkID == "" || len(sources) != len(sourceTypes) || len(sources) != len(inValues) || len(inValues) != len(outValues) {
		http.Error(w, "invalid cut request", http.StatusBadRequest)
		return
	}
	cuts := make([]db.ProductionCut, 0, len(sources))
	for i := range sources {
		source, sourceErr := validateRenderObjectKey(sources[i])
		inMS, inErr := strconv.ParseInt(inValues[i], 10, 64)
		outMS, outErr := strconv.ParseInt(outValues[i], 10, 64)
		if sourceErr != nil || !strings.HasPrefix(source, conference+"/recordings/") || inErr != nil || outErr != nil {
			http.Error(w, fmt.Sprintf("cut %d contains an invalid number", i+1), http.StatusBadRequest)
			return
		}
		cuts = append(cuts, db.ProductionCut{Source: source, SourceType: sourceTypes[i], InMS: inMS, OutMS: outMS})
	}
	if err := h.DB.ReplaceProductionCuts(conference, talkID, cuts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "talk_id": talkID, "cuts": len(cuts)})
}

func (h *Handler) productionObjectLink(ctx context.Context, objectKey string) (string, error) {
	if strings.TrimSpace(h.Remote) == "" {
		return "", fmt.Errorf("Spaces remote is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "link", "--expire", "1h", h.remotePath(objectKey))
	cmd.Env = rcloneEnv(h.RcloneConfig)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("create source preview link: %s", detail)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return "", fmt.Errorf("rclone returned an empty preview link")
	}
	link := lines[len(lines)-1]
	parsed, err := url.Parse(link)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("rclone returned an invalid preview link")
	}
	return link, nil
}
