package musemsp

import (
	"testing"
)

func TestMapModelsNormalizesEntries(t *testing.T) {
	got := MapModels([]Model{
		{ID: "muse-spark-1.3", Label: "muse-spark-1.3", Provider: "meta"},
		{ID: "  ", Label: "blank"},
		{ID: "muse-spark-1.3-contributor", Label: "", Provider: "meta", Default: true},
	})
	if len(got) != 2 {
		t.Fatalf("models = %d, want 2 with the blank id dropped", len(got))
	}
	if got[0].ID != "muse-spark-1.3" || got[0].Label != "muse-spark-1.3" || got[0].Provider != "meta" {
		t.Fatalf("models[0] = %+v", got[0])
	}
	if got[1].Label != got[1].ID || !got[1].IsDefault {
		t.Fatalf("models[1] = %+v, want label fallback and default", got[1])
	}
	for i, row := range got {
		if len(row.Efforts) != len(MuseEffortLevels) || row.DefaultEffort != MuseDefaultEffort {
			t.Fatalf("models[%d] efforts = %v/%q", i, row.Efforts, row.DefaultEffort)
		}
	}
	if MuseDefaultEffort != "high" {
		t.Fatalf("default effort = %q, want the CLI-documented high", MuseDefaultEffort)
	}
}
