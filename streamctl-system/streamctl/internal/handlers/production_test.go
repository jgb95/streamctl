package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

type productionCandidatesStub struct {
	conferences []btcppclient.Conference
	candidates  []btcppclient.Candidate
	err         error
}

func (stub productionCandidatesStub) Conferences(context.Context) ([]btcppclient.Conference, error) {
	return stub.conferences, stub.err
}

func (stub productionCandidatesStub) RecordingCandidates(context.Context, string) ([]btcppclient.Candidate, error) {
	return stub.candidates, stub.err
}

func productionHandlerTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestProductionWorkspaceLoadsCandidatesAndCuts(t *testing.T) {
	database := productionHandlerTestDB(t)
	if err := database.ReplaceProductionCuts("toronto", "talk-1", []db.ProductionCut{{Source: "toronto/recordings/raw/mix/day-1.mp4", SourceType: "video", InMS: 1000, OutMS: 2000}}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		DB: database, funcs: template.FuncMap{},
		BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++", StartsAt: stringPointer("2026-07-22T09:00:00-04:00")}}, candidates: []btcppclient.Candidate{{
			TalkID: "talk-1", Title: "A useful talk", Venue: "one", Eligible: true,
			StartsAt: stringPointer("2026-07-22T10:00:00-04:00"), EndsAt: stringPointer("2026-07-22T10:30:00-04:00"),
		}}},
	}
	response := httptest.NewRecorder()
	h.productionTimestamp(response, httptest.NewRequest(http.MethodGet, "/production/timestamp?conference=toronto", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"Timestamp talks", "Conference workspace", `value="toronto"`, "selected", `onchange="this.form.submit()"`, `/production/media?conference=toronto`, `/production/timestamp?conference=toronto`, "A useful talk", "Day 1", "Main", "Wed, Jul 22, 2026", "10:00 AM–10:30 AM", "1 cut"} {
		if !strings.Contains(body, want) {
			t.Fatalf("workspace omitted %q: %s", want, body)
		}
	}
	if !strings.Contains(response.Header().Get("Set-Cookie"), productionConferenceCookie+"=toronto") {
		t.Fatalf("conference preference was not persisted: %q", response.Header().Get("Set-Cookie"))
	}
	if strings.Contains(body, ">Open</button>") {
		t.Fatalf("timestamp selector retained redundant open button: %s", body)
	}
}

func TestProductionTalkScheduleLabels(t *testing.T) {
	talks := []productionTalkView{
		{Candidate: btcppclient.Candidate{Venue: "1", StartsAt: stringPointer("2026-07-22T10:00:00-04:00")}},
		{Candidate: btcppclient.Candidate{Venue: "stage two", StartsAt: stringPointer("2026-07-23T11:00:00-04:00")}},
		{Candidate: btcppclient.Candidate{Venue: "three", StartsAt: stringPointer("2026-07-24T12:00:00-04:00")}},
	}
	decorateProductionTalks(talks, stringPointer("2026-07-22T09:00:00-04:00"))
	for i, want := range []struct{ day, stage string }{{"Day 1", "Main"}, {"Day 2", "Talks"}, {"Day 3", "Workshop"}} {
		if talks[i].DayLabel != want.day || talks[i].StageLabel != want.stage {
			t.Fatalf("talk %d labels = %q, %q; want %q, %q", i, talks[i].DayLabel, talks[i].StageLabel, want.day, want.stage)
		}
	}
	groups := groupProductionTalks(talks)
	if len(groups) != 3 || groups[1].DayLabel != "Day 2" || groups[1].DateLabel != "Thu, Jul 23, 2026" || len(groups[1].Stages) != 1 || len(groups[1].Stages[0].Talks) != 1 {
		t.Fatalf("unexpected talk groups: %+v", groups)
	}
}

func TestProductionTalksFollowDayStageTimeOrder(t *testing.T) {
	h := &Handler{BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{
		{TalkID: "talks-early", Venue: "two", StartsAt: stringPointer("2026-07-22T09:00:00-04:00")},
		{TalkID: "main-late", Venue: "one", StartsAt: stringPointer("2026-07-22T11:00:00-04:00")},
		{TalkID: "main-early", Venue: "one", StartsAt: stringPointer("2026-07-22T10:00:00-04:00")},
		{TalkID: "next-day", Venue: "one", StartsAt: stringPointer("2026-07-23T08:00:00-04:00")},
	}}}
	talks, err := h.productionTalks(context.Background(), "toronto")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"main-early", "main-late", "talks-early", "next-day"}
	for i := range want {
		if talks[i].TalkID != want[i] {
			t.Fatalf("talk %d = %q; want %q (all=%+v)", i, talks[i].TalkID, want[i], talks)
		}
	}
}

func TestProductionTimestampStartsWithConferenceDropdown(t *testing.T) {
	h := &Handler{funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++"}}}}
	response := httptest.NewRecorder()
	h.productionTimestamp(response, httptest.NewRequest(http.MethodGet, "/production/timestamp", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"<select", "Select a conference", `value="toronto"`, "Choose a conference"} {
		if !strings.Contains(body, want) {
			t.Fatalf("timestamp selector omitted %q: %s", want, body)
		}
	}
}

func TestProductionOverviewKeepsConferenceInNavigation(t *testing.T) {
	h := &Handler{funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++"}}}}
	response := httptest.NewRecorder()
	h.productionHome(response, httptest.NewRequest(http.MethodGet, "/production?conference=toronto", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"Conference workspace", `value="toronto" selected`, `/production/media?conference=toronto`, `/production/timestamp?conference=toronto`} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview omitted %q: %s", want, body)
		}
	}
}

func TestProductionCutIsDedicatedPageWithTalkNavigation(t *testing.T) {
	database := productionHandlerTestDB(t)
	if err := database.ReplaceProductionCuts("toronto", "talk-2", []db.ProductionCut{{Source: "toronto/recordings/main.mp4", SourceType: "video", InMS: 1000, OutMS: 2000}}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{
		{TalkID: "talk-1", Title: "First", Venue: "two", StartsAt: stringPointer("2025-01-01T10:00:00Z")},
		{TalkID: "talk-2", Title: "Second", Venue: "two", StartsAt: stringPointer("2025-01-01T11:00:00Z"), EndsAt: stringPointer("2025-01-01T11:30:00Z"), Speakers: []btcppclient.Speaker{{Name: "Speaker Two"}}},
		{TalkID: "talk-3", Title: "Third", Venue: "two", StartsAt: stringPointer("2025-01-01T12:00:00Z")},
	}}}
	response := httptest.NewRecorder()
	h.productionCut(response, httptest.NewRequest(http.MethodGet, "/production/timestamp/cut?conference=toronto&talk_id=talk-2", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"Second", "Day 1", "Wed, Jan 1, 2025", "Talks", "11:00 AM–11:30 AM", "Speaker Two", "← Previous", "Skip →", "Save &amp; next", `"inMs":1000`, `"index":1`, `"talks":[`, "Back to talks", "global-seek", "/production/media/info", "proxyPath", `preload="auto"`, "seekGlobal", "video.onloadedmetadata", "Loading editing proxy", "sourceFilename", "selectRange", "requestSubmit", "sessionStorage", "persistPlayerState", "settledTimeMs", "initialSeekPending", "loadSource", "showTalk", "history.pushState", "currentTalk.talk_id", "Set In on this source first", "Wait for the preview to finish seeking", "fine-scrubber", "fineWindowMs=5000", "previewSeekTimeoutMs=4000", "queuePreviewSeek", "setPointerCapture", "previewSeekInFlight", "queuedSeekTarget", "recoverPreviewSeek", "video.onseeked=settlePreviewSeek", "seek.onpointerdown", "finishCoarseScrub", "requestAnimationFrame", "↵"} {
		if !strings.Contains(body, want) {
			t.Fatalf("cutter omitted %q: %s", want, body)
		}
	}
	if count := strings.Count(body, ">Choose media</button>"); count != 1 {
		t.Fatalf("cutter rendered %d media selectors; want one: %s", count, body)
	}
	if strings.Contains(body, "Select a talk") {
		t.Fatalf("dedicated cutter retained placeholder: %s", body)
	}
	if strings.Contains(body, "location.assign") {
		t.Fatalf("cutter still reloads between talks: %s", body)
	}
	for _, unwanted := range []string{"next-chunk", "/production/media/preview", "Timed out waiting for video", "locateChunk", "loadChunk", "pendingSeek", "#t="} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("direct cutter retained %q: %s", unwanted, body)
		}
	}
}

func stringPointer(value string) *string { return &value }

func TestSelectedProductionConferenceFallsBackToCookie(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/production/media", nil)
	request.AddCookie(&http.Cookie{Name: productionConferenceCookie, Value: "toronto"})
	if got := selectedProductionConference(request); got != "toronto" {
		t.Fatalf("conference=%q", got)
	}
	request = httptest.NewRequest(http.MethodGet, "/production/media?conference=nairobi", nil)
	request.AddCookie(&http.Cookie{Name: productionConferenceCookie, Value: "toronto"})
	if got := selectedProductionConference(request); got != "nairobi" {
		t.Fatalf("query did not override cookie: %q", got)
	}
}

func TestMediaWorkspaceBrowsesConferenceRecordings(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'main/\\ntalks/\\nclip.mp4\\nnotes.json\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp", funcs: template.FuncMap{}}
	response := httptest.NewRecorder()
	h.mediaWorkspace(response, httptest.NewRequest(http.MethodGet, "/production/media?conference=toronto", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"toronto/recordings/", "main", "talks", "clip.mp4", "notes.json"} {
		if !strings.Contains(body, want) {
			t.Fatalf("media browser omitted %q: %s", want, body)
		}
	}
}

func TestMediaWorkspaceRejectsCrossConferencePrefix(t *testing.T) {
	h := &Handler{Remote: "btcpp:btcpp", funcs: template.FuncMap{}}
	response := httptest.NewRecorder()
	h.mediaWorkspace(response, httptest.NewRequest(http.MethodGet, "/production/media?conference=toronto&prefix=nairobi/recordings/", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMediaBrowserCanNavigateWholeBucket(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'toronto/\\nnairobi/\\nshared.mp4\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	response := httptest.NewRecorder()
	h.mediaBrowse(response, httptest.NewRequest(http.MethodGet, "/production/media/browse?conference=toronto&prefix=", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{`"prefix":""`, `"path":"toronto/"`, `"path":"nairobi/"`, `"path":"shared.mp4"`} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("bucket browser omitted %q: %s", want, response.Body.String())
		}
	}
}

func TestMediaBrowserDefaultsToConferenceRecordings(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'assets/\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	response := httptest.NewRecorder()
	h.mediaBrowse(response, httptest.NewRequest(http.MethodGet, "/production/media/browse?conference=toronto", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"prefix":"toronto/recordings/"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGroupMediaFilesUsesGeneralizedConfRenderChunkRule(t *testing.T) {
	files := groupMediaFiles("toronto/recordings/raw/mix/", []string{
		"camera-0000.mp4", "camera-0001.mp4", "camera-0002.mp4", "single.mp4", "notes.json",
	})
	if len(files) != 3 {
		t.Fatalf("files=%+v", files)
	}
	var sequence *mediaFile
	for i := range files {
		if files[i].SourceType == "chunkedVideo" {
			sequence = &files[i]
		}
	}
	if sequence == nil || sequence.Name != "camera-0000.mp4" || sequence.Kind != "video" || sequence.Path != "toronto/recordings/raw/mix/camera-0000.mp4" || len(sequence.Chunks) != 3 {
		t.Fatalf("sequence=%+v", sequence)
	}
}

func TestMediaFileKind(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{"clip.mp4", "video"}, {"card.PNG", "image"}, {"music.wav", "audio"}, {"notes.json", "other"},
	} {
		if got := mediaFileKind(test.name); got != test.want {
			t.Fatalf("mediaFileKind(%q)=%q want %q", test.name, got, test.want)
		}
	}
}

func TestGroupMediaFilesReturnsEmptyJSONArray(t *testing.T) {
	files := groupMediaFiles("toronto/recordings/", nil)
	if files == nil || len(files) != 0 {
		t.Fatalf("files=%+v, want non-nil empty slice", files)
	}
}

func TestProductionProxyObjectKeyMirrorsSourceDirectory(t *testing.T) {
	source := mediaFile{Path: "toronto/recordings/raw/mix/toronto_01main_100431_0000.mp4", SourceType: "chunkedVideo"}
	if got, want := productionProxyObjectKey("toronto", source), "toronto/recordings/workspace/proxies/raw/mix/toronto_01main_100431.mp4"; got != want {
		t.Fatalf("proxy=%q want %q", got, want)
	}
	standalone := mediaFile{Path: "toronto/recordings/raw/mix/talks.mp4", SourceType: "video"}
	if got, want := productionProxyObjectKey("toronto", standalone), "toronto/recordings/workspace/proxies/raw/mix/talks.mp4"; got != want {
		t.Fatalf("standalone proxy=%q want %q", got, want)
	}
}

func TestLogicalMediaSourcesRecursiveGroupsChunksAndSkipsDerivedMedia(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'raw/mix/camera0000.mp4\\nraw/mix/camera0001.mp4\\nraw/mix/single.mov\\nworkspace/proxies/old.mp4\\nedits/talks/final.mp4\\nassets/bumper.mp4\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	sources, err := h.logicalMediaSourcesRecursive(context.Background(), "toronto/recordings/")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].Path != "toronto/recordings/raw/mix/camera0000.mp4" || sources[0].SourceType != "chunkedVideo" || sources[1].Path != "toronto/recordings/raw/mix/single.mov" {
		t.Fatalf("sources=%+v", sources)
	}
}

func TestLogicalMediaInfoRequiresEditingProxy(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'camera0000.mp4\\ncamera0001.mp4\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	info, err := h.logicalMediaInfo(context.Background(), "toronto/recordings/raw/mix/camera0000.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.SourceType != "chunkedVideo" || info.DurationMS != 0 || info.ProxyPath != "" || !strings.Contains(info.Warning, "Prepare this recording") {
		t.Fatalf("info=%+v", info)
	}
}

func TestLogicalMediaInfoUsesFinishedEditingProxy(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'camera0000.mp4\\ncamera0001.mp4\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	database := productionHandlerTestDB(t)
	source := "toronto/recordings/raw/mix/camera0000.mp4"
	proxy := "toronto/recordings/workspace/proxies/raw/mix/camera.mp4"
	job, _, err := database.EnqueueProductionProxyJob(source, proxy)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := database.ClaimProductionProxyJob()
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if err := database.FinishProductionProxyJob(job.ID, 930123); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, Remote: "btcpp:btcpp"}
	info, err := h.logicalMediaInfo(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if info.ProxyPath != proxy || info.ProxyStatus != "finished" || info.DurationMS != 930123 || info.Warning != "" {
		t.Fatalf("info=%+v", info)
	}
}

func TestRcloneEnvRemovesAmbientFileFilters(t *testing.T) {
	t.Setenv("RCLONE_FILTER_FROM", "/tmp/personal-filter")
	t.Setenv("RCLONE_EXCLUDE", "*.mp4")
	t.Setenv("RCLONE_CONFIG_PASS", "keep-this")
	env := strings.Join(rcloneEnv("/tmp/streamctl-rclone.conf"), "\n")
	for _, unwanted := range []string{"RCLONE_FILTER_FROM=", "RCLONE_EXCLUDE="} {
		if strings.Contains(env, unwanted) {
			t.Fatalf("rclone environment contains %q", unwanted)
		}
	}
	for _, wanted := range []string{"RCLONE_CONFIG_PASS=keep-this", "RCLONE_CONFIG=/tmp/streamctl-rclone.conf"} {
		if !strings.Contains(env, wanted) {
			t.Fatalf("rclone environment does not contain %q", wanted)
		}
	}
}

func TestTransferPercent(t *testing.T) {
	for _, test := range []struct {
		line string
		want int
		ok   bool
	}{
		{"Transferred: 1.000 GiB / 4.000 GiB, 25%, 10 MiB/s, ETA 5m", 25, true},
		{"Transferred: 0 B / 0 B, -, 0 B/s, ETA -", 0, false},
		{"2026/09/02 10:00:00 ERROR : upload failed", 0, false},
	} {
		got, ok := transferPercent(test.line)
		if got != test.want || ok != test.ok {
			t.Errorf("transferPercent(%q) = %d, %v; want %d, %v", test.line, got, ok, test.want, test.ok)
		}
	}
}

func TestProductionCutsSavePersistsFormRanges(t *testing.T) {
	database := productionHandlerTestDB(t)
	h := &Handler{DB: database}
	form := url.Values{
		"conference": {"toronto"}, "talk_id": {"talk-1"},
		"source":      {"toronto/recordings/main0000.mp4", "toronto/recordings/main.mp4"},
		"source_type": {"chunkedVideo", "video"},
		"in_ms":       {"1000", "3000"}, "out_ms": {"2000", "4500"},
	}
	request := httptest.NewRequest(http.MethodPost, "/production/cuts/save", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	h.productionCutsSave(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	cuts, err := database.ListProductionCuts("toronto")
	if err != nil || len(cuts["talk-1"]) != 2 || cuts["talk-1"][1].OutMS != 4500 {
		t.Fatalf("cuts=%+v err=%v", cuts, err)
	}
}
