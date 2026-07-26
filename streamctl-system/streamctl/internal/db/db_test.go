package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestGPUQueueTracksAttemptsAndErrors(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	item, err := database.EnqueueGPUJob("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if item.AttemptCount != 0 || item.LastAttemptAt != nil || item.LastError != "" {
		t.Fatalf("new queue item has unexpected attempt metadata: %#v", item)
	}

	if err := database.MarkGPUQueueRunning(item.ID, "streamctl-gpu-talk.service"); err != nil {
		t.Fatal(err)
	}
	item, err = database.GetGPUQueueItemByRawPath("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "running" || item.AttemptCount != 1 || item.LastAttemptAt == nil || item.LastError != "" {
		t.Fatalf("running queue item has unexpected metadata: %#v", item)
	}

	if err := database.MarkGPUQueueFinished(item.RawPath, "failed", "ssh connection refused"); err != nil {
		t.Fatal(err)
	}
	item, err = database.GetGPUQueueItemByRawPath("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "failed" || item.LastError != "ssh connection refused" {
		t.Fatalf("failed queue item did not retain error: %#v", item)
	}

	item, err = database.EnqueueGPUJob("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "queued" || item.AttemptCount != 1 || item.LastError != "ssh connection refused" {
		t.Fatalf("requeued item should preserve last attempt context: %#v", item)
	}
}

func TestMarkGPUQueueFinishedReportsNoOpenRow(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	item, err := database.EnqueueGPUJob("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueRunning(item.ID, "streamctl-gpu-talk.service"); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueFinished(item.RawPath, "finished", ""); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueFinished(item.RawPath, "failed", "duplicate terminal event"); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows for duplicate terminal update, got %v", err)
	}
}

func TestUpsertGPUJobLogKeepsExistingDetailWhenRefreshIsSparse(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	unit := "streamctl-gpu-talk.service"
	if err := database.UpsertGPUJobLog(GPUJobLog{
		UnitName:    unit,
		RawPath:     "vienna/recordings/edits/talk.mp4",
		Host:        "ssh://root@example",
		Description: "streamctl GPU transcode vienna/recordings/edits/talk.mp4",
		ActiveState: "failed",
		SubState:    "failed",
		Result:      "exit-code",
		Journal:     "rclone: object not found",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertGPUJobLog(GPUJobLog{
		UnitName: unit,
		Error:    "remote job metadata: connection refused",
	}); err != nil {
		t.Fatal(err)
	}

	job, err := database.GetGPUJobLog(unit)
	if err != nil {
		t.Fatal(err)
	}
	if job.RawPath != "vienna/recordings/edits/talk.mp4" || job.Result != "exit-code" || job.Journal != "rclone: object not found" {
		t.Fatalf("sparse refresh should preserve previous job detail: %#v", job)
	}
	if job.Error != "remote job metadata: connection refused" {
		t.Fatalf("sparse refresh should record new error: %#v", job)
	}
}

func TestResetGPUJobForRetryHandlesQueuedJobs(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	item, err := database.EnqueueGPUJob("vienna/recordings/edits/talks/stuck.mov")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ResetGPUJobForRetry(item.RawPath, "manual retry"); err != nil {
		t.Fatal(err)
	}
	item, err = database.GetGPUQueueItemByRawPath(item.RawPath)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "queued" || item.UnitName != "" || item.LastError != "manual retry" || item.AttemptCount != 0 {
		t.Fatalf("queued retry reset produced unexpected item: %#v", item)
	}
}

func TestVisibleGPUQueueItemsExcludeFinished(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	finished, err := database.EnqueueGPUJob("vienna/recordings/edits/finished.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueFinished(finished.RawPath, "finished", ""); err != nil {
		t.Fatal(err)
	}
	failed, err := database.EnqueueGPUJob("vienna/recordings/edits/failed.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueFinished(failed.RawPath, "failed", "boom"); err != nil {
		t.Fatal(err)
	}

	visible, err := database.ListVisibleGPUQueueItems(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].RawPath != failed.RawPath {
		t.Fatalf("visible items should include failed but not finished: %#v", visible)
	}
	failedItems, err := database.ListFailedGPUQueueItems(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(failedItems) != 1 || failedItems[0].RawPath != failed.RawPath {
		t.Fatalf("failed items mismatch: %#v", failedItems)
	}
}
