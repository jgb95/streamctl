package db

import (
	"database/sql"
	"errors"
	"testing"
)

func TestProductionRenderCRUDAndGeneratedUniqueness(t *testing.T) {
	database := productionTestDB(t)
	templateID, err := database.CreateProductionTemplate("toronto", "Talk", `{"version":1,"settings":{},"segments":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"version":1,"settings":{},"jobs":[{"id":"talk","segments":[]}]}`
	id, created, err := database.CreateProductionRender("toronto", "Talk one", manifest, &templateID, "talk-1")
	if err != nil || !created || id == 0 {
		t.Fatalf("create id=%d created=%v err=%v", id, created, err)
	}
	if _, created, err := database.CreateProductionRender("toronto", "Talk one", manifest, &templateID, "talk-1"); err != nil || created {
		t.Fatalf("duplicate generated render created=%v err=%v", created, err)
	}
	if err := database.UpdateProductionRender(id, "toronto", "Talk one edited", manifest); err != nil {
		t.Fatal(err)
	}
	item, err := database.ProductionRender(id, "toronto")
	if err != nil || item.Name != "Talk one edited" || !item.TemplateID.Valid || item.TalkID != "talk-1" {
		t.Fatalf("item=%+v err=%v", item, err)
	}
	items, err := database.ListProductionRenders("toronto")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if err := database.ArchiveProductionRender(id, "toronto"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ProductionRender(id, "toronto"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("archived render remained visible: %v", err)
	}
	restoredID, restored, err := database.CreateProductionRender("toronto", "Talk one restored", manifest, &templateID, "talk-1")
	if err != nil || !restored || restoredID != id {
		t.Fatalf("restore id=%d restored=%v err=%v", restoredID, restored, err)
	}
}

func TestProductionRenderQueueLifecycle(t *testing.T) {
	database := productionTestDB(t)
	manifest := `{"version":1,"settings":{},"jobs":[{"id":"talk","segments":[{"type":"video","src":"toronto/recordings/raw/talk.mp4"}]}]}`
	id, _, err := database.CreateProductionRender("toronto", "Talk", manifest, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	job, err := database.EnqueueProductionRender(id, "Talk", manifest)
	if err != nil || !job.ProductionRenderID.Valid || job.ProductionRenderID.Int64 != id {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if _, err := database.EnqueueProductionRender(id, "Talk", manifest); !errors.Is(err, ErrRenderAlreadyActive) {
		t.Fatalf("second active job error=%v", err)
	}
	states, err := database.ProductionRenderQueueStates("toronto")
	if err != nil || states[id].Latest == nil || states[id].Latest.ID != job.ID {
		t.Fatalf("states=%+v err=%v", states, err)
	}
	if err := database.MarkRenderQueueRunning(job.ID, "render.service"); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueFinished(job.ID, "finished", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.EnqueueProductionRender(id, "Talk", manifest); err != nil {
		t.Fatalf("queue after completion: %v", err)
	}
	states, err = database.ProductionRenderQueueStates("toronto")
	if err != nil || !states[id].HasFinished || states[id].Latest.ID == job.ID {
		t.Fatalf("updated states=%+v err=%v", states, err)
	}
	if err := database.ArchiveProductionRender(id, "toronto"); err != nil {
		t.Fatal(err)
	}
	preserved, err := database.GetRenderQueueItem(job.ID)
	if err != nil || !preserved.ProductionRenderID.Valid || preserved.ProductionRenderID.Int64 != id {
		t.Fatalf("preserved job=%+v err=%v", preserved, err)
	}
}
