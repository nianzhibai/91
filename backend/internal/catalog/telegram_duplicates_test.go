package catalog

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func completeDuplicateFixture(t *testing.T, c *Catalog, messageID int64, id string) {
	t.Helper()
	ctx := context.Background()
	enqueueTelegram(t, c, messageID, id, id)
	if err := c.TransitionRemoteUploadJob(ctx, id, RemoteUploadQueued, RemoteUploadSaving); err != nil {
		t.Fatal(err)
	}
	video := &Video{ID: "video-" + id, DriveID: "local-upload", FileID: id + ".mp4", FileName: id + ".mp4", Title: "已保存的视频", Ext: "mp4", Size: 5}
	if err := c.FinalizeRemoteUpload(ctx, id, video, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateReceiptUsesAdmissionTimeAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	completeDuplicateFixture(t, c, 1, "saved")
	enqueueTelegram(t, c, 2, "saved", "unused-duplicate")
	enqueueTelegram(t, c, 3, "pending", "pending")
	enqueueTelegram(t, c, 4, "pending", "unused-pending")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 4 {
		t.Fatalf("missing receipts: %+v %v", receipts, err)
	}
	for _, r := range receipts {
		var want []string
		if r.MessageID == 2 {
			want = []string{"saved"}
		}
		if !slices.Equal(r.DuplicateJobIDs, want) {
			t.Fatalf("receipt %d misclassified a new or pending video: %+v", r.MessageID, r)
		}
		if r.MessageID == 2 {
			if err := c.SaveTelegramNotification(ctx, r, 55, RemoteUploadCompleted, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := c.db.Exec(`UPDATE telegram_receipts SET next_attempt=0`); err != nil {
		t.Fatal(err)
	}
	if pending, err := c.PendingTelegramReceipts(ctx, 123); err != nil || len(pending) != 3 {
		t.Fatalf("duplicate notice repeated after delivery: %+v %v", pending, err)
	}
	if jobs, err := c.ListImportJobs(ctx, "telegram", "", 0, 30); err != nil || len(jobs) != 2 {
		t.Fatalf("duplicate created a new task: %+v %v", jobs, err)
	}
	if err := c.InvalidateMissingTelegramVideo(ctx, 123, "saved"); err != nil {
		t.Fatal(err)
	}
	enqueueTelegram(t, c, 5, "saved", "replacement")
	receipts, err = c.PendingTelegramReceipts(ctx, 123)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range receipts {
		if r.MessageID == 5 {
			found = true
			if r.JobID != "replacement" || len(r.DuplicateJobIDs) != 0 {
				t.Fatalf("missing file was incorrectly skipped: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("missing replacement receipt")
	}
}

func TestMediaGroupDuplicatesAreSharedByLinksAndForwards(t *testing.T) {
	for _, kind := range []string{"link", "forward"} {
		for _, allDuplicate := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/mixed", true: "/all duplicate"}[allDuplicate], func(t *testing.T) {
				c := telegramTestCatalog(t)
				ctx := context.Background()
				completeDuplicateFixture(t, c, 1, "saved-a")
				if allDuplicate {
					completeDuplicateFixture(t, c, 2, "saved-b")
				}
				inputs := linkedAlbumImports()
				var group TelegramMediaGroup
				if kind == "forward" {
					for _, id := range []int64{10, 11} {
						if err := c.StageTelegramMediaGroupUpdate(ctx, stagedAlbumUpdate(id)); err != nil {
							t.Fatal(err)
						}
					}
					group = pendingAlbum(t, c)
					inputs = albumImports(group)
				}
				inputs[0].Source.UniqueID = "saved-a"
				if allDuplicate {
					inputs[1].Source.UniqueID = "saved-b"
				}
				if kind == "link" {
					r := TelegramReceipt{BotID: 123, ChatID: 42, SenderID: 42, MessageID: 10, UpdateID: 10}
					if err := c.AcceptTelegramMediaLink(ctx, r, inputs, 100); err != nil {
						t.Fatal(err)
					}
				} else if accepted, err := c.AcceptTelegramMediaGroup(ctx, group, inputs, 100); err != nil || !accepted {
					t.Fatalf("forwarded group rejected: %v %v", accepted, err)
				}
				receipts, err := c.PendingTelegramReceipts(ctx, 123)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"saved-a"}
				if allDuplicate {
					want = append(want, "saved-b")
				}
				found := false
				for _, r := range receipts {
					if r.MessageID == 10 {
						found = true
						if len(r.JobIDs) != 2 || !slices.Equal(r.DuplicateJobIDs, want) {
							t.Fatalf("group duplicate metadata incorrect: %+v", r)
						}
					}
				}
				if !found {
					t.Fatal("missing group receipt")
				}
				if jobs, err := c.ListImportJobs(ctx, "telegram", "", 0, 30); err != nil || len(jobs) != 2 {
					t.Fatalf("group resaved duplicate videos: %+v %v", jobs, err)
				}
			})
		}
	}
}
