package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRemoteUploadJobLifecycleClearsSensitiveURL(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	seedCustomTagRules(t, cat)

	defer cat.Close()

	job, err := cat.CreateRemoteUploadJob(
		ctx,
		"remote-job-1",
		"https://cdn.example/video.mp4?token=secret",
		"cdn.example/video.mp4",
		"显式标题",
		[]string{"奶子"},
	)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job.State != RemoteUploadQueued || !job.CanCancel() {
		t.Fatalf("created job = %#v", job)
	}

	claimed, err := cat.ClaimNextRemoteUploadJob(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != job.ID || claimed.State != RemoteUploadDownloading {
		t.Fatalf("claimed job = %#v", claimed)
	}
	if err := cat.UpdateRemoteUploadProgress(ctx, job.ID, 128, 256); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := cat.TransitionRemoteUploadJob(
		ctx,
		job.ID,
		RemoteUploadDownloading,
		RemoteUploadValidating,
	); err != nil {
		t.Fatalf("validating: %v", err)
	}
	if err := cat.TransitionRemoteUploadJob(
		ctx,
		job.ID,
		RemoteUploadValidating,
		RemoteUploadSaving,
	); err != nil {
		t.Fatalf("saving: %v", err)
	}
	if err := cat.PrepareRemoteUploadSaving(
		ctx,
		job.ID,
		".remote-job-1.part",
		"显式标题.mp4",
		"local-upload-upload-1",
		"显式标题",
	); err != nil {
		t.Fatalf("prepare saving: %v", err)
	}

	now := time.Now()
	video := &Video{
		ID:          "local-upload-upload-1",
		DriveID:     "local-upload",
		FileID:      "显式标题.mp4",
		FileName:    "显式标题.mp4",
		Title:       "显式标题",
		Author:      "用户上传",
		Size:        128,
		Ext:         "mp4",
		PublishedAt: now,
		CreatedAt:   now,
	}
	if err := cat.FinalizeRemoteUpload(ctx, job.ID, video, []string{"奶子"}, nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	savedJob, err := cat.GetRemoteUploadJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get completed job: %v", err)
	}
	if savedJob.State != RemoteUploadCompleted ||
		savedJob.SourceURL != "" ||
		savedJob.CompletedVideoID != video.ID ||
		savedJob.CanCancel() {
		t.Fatalf("completed job = %#v", savedJob)
	}
	savedVideo, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if savedVideo.Title != "显式标题" ||
		len(savedVideo.Tags) != 1 ||
		savedVideo.Tags[0] != "奶子" {
		t.Fatalf("saved video = %#v", savedVideo)
	}
	if _, err := cat.CancelRemoteUploadJob(ctx, job.ID); !errors.Is(err, ErrRemoteUploadTerminal) {
		t.Fatalf("cancel completed error = %v", err)
	}
}

func TestRecoverRemoteUploadJobsRequeuesOrFinishesCancellation(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	createAndClaim := func(id string) *RemoteUploadJob {
		t.Helper()
		if _, err := cat.CreateRemoteUploadJob(
			ctx,
			id,
			"https://cdn.example/"+id+".mp4?signature=private",
			"cdn.example/"+id+".mp4",
			"",
			nil,
		); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		job, err := cat.ClaimNextRemoteUploadJob(ctx)
		if err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		return job
	}

	requeue := createAndClaim("a-requeue")
	if err := cat.SetRemoteUploadTempFile(ctx, requeue.ID, ".remote-a.part"); err != nil {
		t.Fatalf("set requeue temp: %v", err)
	}
	cancel := createAndClaim("b-cancel")
	if err := cat.SetRemoteUploadTempFile(ctx, cancel.ID, ".remote-b.part"); err != nil {
		t.Fatalf("set cancel temp: %v", err)
	}
	if _, err := cat.CancelRemoteUploadJob(ctx, cancel.ID); err != nil {
		t.Fatalf("request cancellation: %v", err)
	}

	refs, err := cat.RecoverRemoteUploadJobs(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("cleanup refs = %#v", refs)
	}
	requeued, err := cat.GetRemoteUploadJob(ctx, requeue.ID)
	if err != nil {
		t.Fatalf("get requeued: %v", err)
	}
	if requeued.State != RemoteUploadQueued ||
		requeued.SourceURL == "" ||
		requeued.BytesDownloaded != 0 ||
		!requeued.StartedAt.IsZero() {
		t.Fatalf("requeued = %#v", requeued)
	}
	canceled, err := cat.GetRemoteUploadJob(ctx, cancel.ID)
	if err != nil {
		t.Fatalf("get canceled: %v", err)
	}
	if canceled.State != RemoteUploadCanceled ||
		canceled.SourceURL != "" ||
		!canceled.Terminal() {
		t.Fatalf("canceled = %#v", canceled)
	}
}
