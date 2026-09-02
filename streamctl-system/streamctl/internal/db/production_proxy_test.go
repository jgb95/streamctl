package db

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestProductionProxyQueueLifecycleAndRetry(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	source := "toronto/recordings/raw/mix/toronto_01main_100431_0000.mp4"
	proxy := "toronto/recordings/production/proxies/mix/toronto_01main_100431.mp4"
	job, queued, err := database.EnqueueProductionProxyJob(source, proxy)
	if err != nil || !queued || job.Status != "queued" {
		t.Fatalf("enqueue job=%+v queued=%v err=%v", job, queued, err)
	}
	claimed, err := database.ClaimProductionProxyJob()
	if err != nil || claimed.ID != job.ID || claimed.Status != "running" || claimed.Attempts != 1 {
		t.Fatalf("claim job=%+v err=%v", claimed, err)
	}
	if err := database.FailProductionProxyJob(job.ID, errors.New("test failure")); err != nil {
		t.Fatal(err)
	}
	failed, queued, err := database.EnqueueProductionProxyJob(source, proxy)
	if err != nil || !queued || failed.Status != "queued" {
		t.Fatalf("retry job=%+v queued=%v err=%v", failed, queued, err)
	}
	claimed, err = database.ClaimProductionProxyJob()
	if err != nil {
		t.Fatal(err)
	}
	if err := database.FinishProductionProxyJob(claimed.ID, 930123); err != nil {
		t.Fatal(err)
	}
	finished, queued, err := database.EnqueueProductionProxyJob(source, proxy)
	if err != nil || queued || finished.Status != "finished" || finished.DurationMS != 930123 {
		t.Fatalf("finished job=%+v queued=%v err=%v", finished, queued, err)
	}
}
