package db

import (
	"path/filepath"
	"testing"
)

func productionTestDB(t *testing.T) *DB {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestProductionCutsReplaceOrderedRanges(t *testing.T) {
	database := productionTestDB(t)
	cuts := []ProductionCut{
		{Source: "toronto/recordings/raw/mix/day-1_0000.mp4", SourceType: "chunkedVideo", InMS: 1000, OutMS: 2000},
		{Source: "toronto/recordings/raw/mix/day-1.mp4", SourceType: "video", InMS: 3000, OutMS: 4500},
	}
	if err := database.ReplaceProductionCuts("toronto", "talk-1", cuts); err != nil {
		t.Fatal(err)
	}
	byTalk, err := database.ListProductionCuts("toronto")
	if err != nil {
		t.Fatal(err)
	}
	got := byTalk["talk-1"]
	if len(got) != 2 || got[0].Position != 0 || got[0].SourceType != "chunkedVideo" || got[1].Source != cuts[1].Source || got[1].OutMS != 4500 {
		t.Fatalf("unexpected cuts: %+v", got)
	}
	if err := database.ReplaceProductionCuts("toronto", "talk-1", cuts[:1]); err != nil {
		t.Fatal(err)
	}
	byTalk, err = database.ListProductionCuts("toronto")
	if err != nil || len(byTalk["talk-1"]) != 1 {
		t.Fatalf("replacement cuts=%+v err=%v", byTalk, err)
	}
}

func TestProductionCutsRejectInvalidOrCrossConferenceMedia(t *testing.T) {
	database := productionTestDB(t)
	if err := database.ReplaceProductionCuts("toronto", "talk-1", []ProductionCut{{Source: "nairobi/recordings/main.mp4", InMS: 0, OutMS: 10}}); err == nil {
		t.Fatal("accepted media from another conference")
	}
	if err := database.ReplaceProductionCuts("toronto", "talk-1", []ProductionCut{{Source: "toronto/recordings/main.mp4", InMS: 10, OutMS: 10}}); err == nil {
		t.Fatal("accepted empty cut")
	}
}
