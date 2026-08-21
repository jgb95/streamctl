package handlers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"streamctl/internal/db"
)

func TestGPUFailureJournalSummaryReturnsLastNonblankLines(t *testing.T) {
	journal := "\nfirst\n\nsecond\nthird\nfourth\nfifth\nsixth\n"

	got := gpuFailureJournalSummary(journal)
	want := "second\nthird\nfourth\nfifth\nsixth"
	if got != want {
		t.Fatalf("summary mismatch:\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestReconcileStaleGPUQueueDoesNotRequeueWhileWorkerIsStarting(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueGPUJob("toronto/recordings/edits/livestream/day1.mp4")
	if err != nil {
		t.Fatal(err)
	}
	unit := gpuTranscodeUnitName(item.RawPath)
	if err := database.MarkGPUQueueRunning(item.ID, unit); err != nil {
		t.Fatal(err)
	}
	running, err := database.GetGPUQueueItemByRawPath(item.RawPath)
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{DB: database}
	h.reconcileStaleGPUQueue(gpuWorkerView{Managed: true, Status: "starting"}, nil, []db.GPUJobQueueItem{*running})

	got, err := database.GetGPUQueueItemByRawPath(item.RawPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" || got.UnitName != unit {
		t.Fatalf("transient worker state requeued a live job: %#v", got)
	}
}

func TestReconcileStaleGPUQueueRequeuesWhenManagedWorkerIsGone(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueGPUJob("toronto/recordings/edits/livestream/day1.mp4")
	if err != nil {
		t.Fatal(err)
	}
	unit := gpuTranscodeUnitName(item.RawPath)
	if err := database.MarkGPUQueueRunning(item.ID, unit); err != nil {
		t.Fatal(err)
	}
	running, err := database.GetGPUQueueItemByRawPath(item.RawPath)
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{DB: database}
	h.reconcileStaleGPUQueue(gpuWorkerView{Managed: true, Status: "not found"}, []gpuJobView{{
		UnitName:    unit,
		RawPath:     item.RawPath,
		ActiveState: "running",
	}}, []db.GPUJobQueueItem{*running})

	got, err := database.GetGPUQueueItemByRawPath(item.RawPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" {
		t.Fatalf("missing managed worker did not requeue stale job: %#v", got)
	}
}

func TestRemoteGPUTranscodeCommandLocksDuplicateFallbackLaunches(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock is required by the worker fallback")
	}
	root := t.TempDir()
	countFile := filepath.Join(root, "starts")
	worker := filepath.Join(root, "worker.sh")
	workerScript := "#!/bin/sh\nprintf 'started\\n' >> " + shellQuote(countFile) + "\nsleep 10\n"
	if err := os.WriteFile(worker, []byte(workerScript), 0755); err != nil {
		t.Fatal(err)
	}
	unit := "streamctl-gpu-lock-test.service"
	remote := remoteGPUTranscodeCommand(unit, worker, "toronto/recordings/edits/livestream/day1.mp4")
	run := func() {
		cmd := exec.Command("bash", "-c", remote)
		cmd.Env = append(os.Environ(), "STREAMCTL_GPU_JOB_ROOT="+root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("launch failed: %v: %s", err, out)
		}
	}
	run()
	waitForGPUStartCount(t, countFile, 1)
	run()
	time.Sleep(100 * time.Millisecond)
	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "started\n"); got != 1 {
		t.Fatalf("duplicate launch ran %d worker processes, want 1", got)
	}

	pidData, err := os.ReadFile(filepath.Join(root, unit, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}

func openGPUQueueTestDB(t *testing.T) *db.DB {
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

func waitForGPUStartCount(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Count(string(data), "started\n") >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("worker did not start %d time(s)", want)
}
