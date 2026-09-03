package db

import (
	"database/sql"
	"errors"
	"testing"
)

func TestProductionTemplatesCRUD(t *testing.T) {
	database := productionTestDB(t)
	id, err := database.CreateProductionTemplate("toronto", "Standard talk", `{"version":1,"settings":{},"segments":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateProductionTemplate("nairobi", "Other conference", `{"version":1,"settings":{},"segments":[]}`); err != nil {
		t.Fatal(err)
	}
	items, err := database.ListProductionTemplates("toronto")
	if err != nil || len(items) != 1 || items[0].ID != id || items[0].Name != "Standard talk" {
		t.Fatalf("templates=%+v err=%v", items, err)
	}
	if err := database.UpdateProductionTemplate(id, "toronto", "Talk v2", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts"}]}`); err != nil {
		t.Fatal(err)
	}
	item, err := database.ProductionTemplate(id, "toronto")
	if err != nil || item.Name != "Talk v2" || item.JSON == "" {
		t.Fatalf("template=%+v err=%v", item, err)
	}
	if _, err := database.ProductionTemplate(id, "nairobi"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-conference lookup err=%v", err)
	}
	if err := database.DeleteProductionTemplate(id, "toronto"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ProductionTemplate(id, "toronto"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted template err=%v", err)
	}
}

func TestProductionTemplatesRequireValues(t *testing.T) {
	database := productionTestDB(t)
	if _, err := database.CreateProductionTemplate("", "Talk", `{}`); err == nil {
		t.Fatal("accepted empty conference")
	}
	if _, err := database.CreateProductionTemplate("toronto", "", `{}`); err == nil {
		t.Fatal("accepted empty name")
	}
}
