package handlers

import "testing"

func TestGPUFailureJournalSummaryReturnsLastNonblankLines(t *testing.T) {
	journal := "\nfirst\n\nsecond\nthird\nfourth\nfifth\nsixth\n"

	got := gpuFailureJournalSummary(journal)
	want := "second\nthird\nfourth\nfifth\nsixth"
	if got != want {
		t.Fatalf("summary mismatch:\nwant:\n%s\ngot:\n%s", want, got)
	}
}
