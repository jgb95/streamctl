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
	if err := database.ReplaceProductionCuts("toronto", "talk-1", []db.ProductionCut{{Source: "toronto/recordings/main/day-1.mp4", SourceType: "video", InMS: 1000, OutMS: 2000}}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		DB: database, funcs: template.FuncMap{},
		BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++"}}, candidates: []btcppclient.Candidate{{
			TalkID: "talk-1", Title: "A useful talk", Venue: "Main", Eligible: true,
		}}},
	}
	response := httptest.NewRecorder()
	h.productionTimestamp(response, httptest.NewRequest(http.MethodGet, "/production/timestamp?conference=toronto", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"Timestamp talks", "Conference workspace", `value="toronto"`, "selected", `onchange="this.form.submit()"`, `/production/media?conference=toronto`, `/production/timestamp?conference=toronto`, "A useful talk", "1 cut"} {
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
		{TalkID: "talk-1", Title: "First", StartsAt: stringPointer("2025-01-01T10:00:00Z")},
		{TalkID: "talk-2", Title: "Second", StartsAt: stringPointer("2025-01-01T11:00:00Z")},
		{TalkID: "talk-3", Title: "Third", StartsAt: stringPointer("2025-01-01T12:00:00Z")},
	}}}
	response := httptest.NewRecorder()
	h.productionCut(response, httptest.NewRequest(http.MethodGet, "/production/timestamp/cut?conference=toronto&talk_id=talk-2", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"Second", "Previous", "Next", `"inMs":1000`, "All talks", "global-seek", "/production/media/info", "locateChunk", "loadChunk", "timing unavailable", ".editor video[hidden]{display:none}"} {
		if !strings.Contains(body, want) {
			t.Fatalf("cutter omitted %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Select a talk") {
		t.Fatalf("dedicated cutter retained placeholder: %s", body)
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

func TestGroupMediaFilesUsesGeneralizedConfRenderChunkRule(t *testing.T) {
	files := groupMediaFiles("toronto/recordings/main/", []string{
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
	if sequence == nil || sequence.Path != "toronto/recordings/main/camera-0000.mp4" || len(sequence.Chunks) != 3 {
		t.Fatalf("sequence=%+v", sequence)
	}
}

func TestGroupMediaFilesReturnsEmptyJSONArray(t *testing.T) {
	files := groupMediaFiles("toronto/recordings/", nil)
	if files == nil || len(files) != 0 {
		t.Fatalf("files=%+v, want non-nil empty slice", files)
	}
}

func TestLogicalMediaInfoUsesVMixLogsAndCaches(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte(`#!/bin/sh
if [ "$1" = "lsf" ]; then
  printf 'camera0000.mp4\ncamera0001.mp4\n'
else
  case "$*" in
    *camera0000.mp4.log*) printf 'RestartInterval: 10\n' ;;
    *camera0001.mp4.log*) printf 'Duration: 00:15:30\n' ;;
    *) exit 90 ;;
  esac
fi
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	info, err := h.logicalMediaInfo(context.Background(), "toronto/recordings/main/camera0000.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.SourceType != "chunkedVideo" || info.TimingSource != "vMix logs" || info.DurationMS != 930000 || len(info.Chunks) != 2 || info.Chunks[0].DurationMS != 600000 || info.Chunks[1].DurationMS != 330000 {
		t.Fatalf("info=%+v", info)
	}
	if err := os.Remove(rclone); err != nil {
		t.Fatal(err)
	}
	if cached, err := h.logicalMediaInfo(context.Background(), info.Path); err != nil || cached.DurationMS != info.DurationMS {
		t.Fatalf("cached=%+v err=%v", cached, err)
	}
}

func TestLogicalMediaInfoFallsBackWithoutVMixLogs(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nif [ \"$1\" = lsf ]; then printf 'camera0000.mp4\\ncamera0001.mp4\\n'; else exit 1; fi\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	info, err := h.logicalMediaInfo(context.Background(), "toronto/recordings/main/camera0000.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.DurationMS != 0 || len(info.Chunks) != 2 || !strings.Contains(info.Warning, "Sequence-wide seeking") {
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
