package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"streamctl/internal/btcppclient"
)

func TestValidateProductionTemplate(t *testing.T) {
	valid := `{
		"version": 1,
		"settings": {"width": 1920, "videoEncoder": "auto", "videoBitrate": "6800k"},
		"segments": [
			{"type":"image","src":"toronto/recordings/assets/card.png","durationMs":4000},
			{"type":"video","src":"toronto/recordings/assets/intro.mp4","in":"00:00:01.000","out":"00:00:03.000","overlay":"toronto/recordings/assets/bug.png","transcribe":false},
			{"type":"streamctl.talkCuts","overlay":"toronto/recordings/assets/bug.png"}
		]
	}`
	canonical, err := validateProductionTemplate([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version": 1`, `"type": "streamctl.talkCuts"`, `"src": "toronto/recordings/assets/intro.mp4"`} {
		if !strings.Contains(canonical, want) {
			t.Fatalf("canonical template omitted %q: %s", want, canonical)
		}
	}
}

func TestValidateProductionTemplateRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"missing settings", `{"version":1,"segments":[]}`},
		{"missing segments", `{"version":1,"settings":{}}`},
		{"unknown setting", `{"version":1,"settings":{"wat":1},"segments":[]}`},
		{"unknown static field", `{"version":1,"settings":{},"segments":[{"type":"image","src":"toronto/recordings/card.png","wat":1}]}`},
		{"missing source", `{"version":1,"settings":{},"segments":[{"type":"video"}]}`},
		{"absolute source", `{"version":1,"settings":{},"segments":[{"type":"video","src":"/recordings/intro.mp4"}]}`},
		{"video with image source", `{"version":1,"settings":{},"segments":[{"type":"video","src":"toronto/recordings/card.png"}]}`},
		{"image with video source", `{"version":1,"settings":{},"segments":[{"type":"image","src":"toronto/recordings/intro.mp4"}]}`},
		{"video overlay", `{"version":1,"settings":{},"segments":[{"type":"video","src":"toronto/recordings/intro.mp4","overlay":"toronto/recordings/bug.mp4"}]}`},
		{"video used as audio", `{"version":1,"settings":{},"segments":[{"type":"video","src":"toronto/recordings/intro.mp4","audio":{"src":"toronto/recordings/music.mp4"}}]}`},
		{"dynamic source", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts","src":"toronto/recordings/raw.mp4"}]}`},
		{"dynamic transcription", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts","transcribe":true}]}`},
		{"bad window", `{"version":1,"settings":{},"segments":[{"type":"video","src":"toronto/recordings/raw.mp4","in":"00:00:05.000","out":"00:00:04.000"}]}`},
		{"unknown type", `{"version":1,"settings":{},"segments":[{"type":"streamctl.magic"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateProductionTemplate([]byte(test.raw)); err == nil {
				t.Fatalf("accepted %s", test.raw)
			}
		})
	}
}

func TestValidateProductionTemplateAllowsAnyBucketObject(t *testing.T) {
	for _, source := range []string{"nairobi/recordings/assets/intro.mp4", "shared.mp4"} {
		raw := `{"version":1,"settings":{},"segments":[{"type":"video","src":"` + source + `"}]}`
		if _, err := validateProductionTemplate([]byte(raw)); err != nil {
			t.Fatalf("source %q: %v", source, err)
		}
	}
}

func TestProductionTemplatesListAndEditor(t *testing.T) {
	database := productionHandlerTestDB(t)
	id, err := database.CreateProductionTemplate("toronto", "Standard talk", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++"}}}}
	response := httptest.NewRecorder()
	h.productionTemplates(response, httptest.NewRequest(http.MethodGet, "/production/templates?conference=toronto", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{"Templates", "Standard talk", "1 segment", "/production/templates/edit?conference=toronto", ">Talks</a>"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("template list omitted %q: %s", want, response.Body.String())
		}
	}

	response = httptest.NewRecorder()
	editURL := productionTemplateURL("toronto", id)
	h.productionTemplateEdit(response, httptest.NewRequest(http.MethodGet, editURL, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{"Standard talk", "Render settings", "+ Add segment", "A video file or numbered sequence", "The saved source ranges for each talk", "streamctl.talkCuts", "Choose media", "Save template", "Drag to reorder", "add.onclick=openSegmentDialog", "segment.type=file.sourceType", "createProductionMediaBrowser", "allowBucketRoot:true", "kind!=='video'||file.proxyStatus==='finished'"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("template editor omitted %q: %s", want, response.Body.String())
		}
	}
}

func TestProductionTemplateSaveValidatesAndPersists(t *testing.T) {
	database := productionHandlerTestDB(t)
	id, err := database.CreateProductionTemplate("toronto", "Draft", defaultProductionTemplateJSON)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database}
	definition := `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts","overlay":"toronto/recordings/assets/bug.png"}]}`
	form := url.Values{"conference": {"toronto"}, "id": {"1"}, "name": {"Standard talk"}, "template": {definition}}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/production/templates/save", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.productionTemplateSave(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	item, err := database.ProductionTemplate(id, "toronto")
	if err != nil || item.Name != "Standard talk" || !strings.Contains(item.JSON, "streamctl.talkCuts") {
		t.Fatalf("template=%+v err=%v", item, err)
	}
}
