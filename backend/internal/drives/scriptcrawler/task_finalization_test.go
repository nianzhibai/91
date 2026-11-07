package scriptcrawler

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/crawljob"
	"github.com/nianzhibai/91/backend/internal/persistence"
)

func TestCrawlerTerminalSaveRetriesBeforeReleasingTask(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name, wantState := "completed", "completed"
		if canceled {
			name, wantState = "canceled", "canceled"
		}
		t.Run(name, func(t *testing.T) {
			c := newRuntimeTestCrawler(t, `c=read();send(c,"page",items=[],next_cursor=None);stop()`, ProtocolV3, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			task, err := c.Prepare(ctx, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(c.cfg.ScriptPath), "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			// RAISE(FAIL) preserves the attempt counter while rejecting the
			// terminal update, so the test observes real failed database writes.
			if _, err := db.Exec(`CREATE TABLE terminal_save_attempts (count INTEGER);
INSERT INTO terminal_save_attempts VALUES (0);
CREATE TRIGGER reject_terminal BEFORE UPDATE ON crawler_tasks
WHEN NEW.state NOT IN ('queued','running') BEGIN
 UPDATE terminal_save_attempts SET count=count+1;
 SELECT RAISE(FAIL,'temporary terminal write failure');
END;`); err != nil {
				t.Fatal(err)
			}
			var terminalProgress atomic.Bool
			c.cfg.OnProgress = func(r crawljob.Result) {
				if r.State == "queued" || r.State == "running" {
					return
				}
				stored, err := c.cfg.Catalog.GetCrawlerTask(context.Background(), r.DriveID, r.TaskID)
				if err != nil || stored.State != r.State {
					t.Errorf("terminal progress preceded persistence: %+v %v", stored, err)
				}
				if _, err := os.Stat(task.dir); err != nil {
					t.Errorf("workspace removed before terminal persistence: %v", err)
				}
				terminalProgress.Store(true)
			}
			if canceled {
				cancel()
			}
			type outcome struct {
				result *crawljob.Result
				err    error
			}
			finished := make(chan outcome, 1)
			exited := make(chan struct{})
			go func() {
				defer close(exited)
				r, err := c.RunTask(ctx, task, nil)
				finished <- outcome{r, err}
			}()
			defer func() {
				// Restore storage even after an assertion fails, then join the
				// task before its catalog and temporary workspace are closed.
				_, _ = db.Exec(`DROP TRIGGER IF EXISTS reject_terminal`)
				select {
				case <-exited:
				case <-time.After(10 * time.Second):
					t.Error("task did not finish after storage recovered")
				}
			}()
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var attempts int
				if err := db.QueryRow(`SELECT count FROM terminal_save_attempts`).Scan(&attempts); err != nil {
					t.Fatal(err)
				}
				if attempts >= 3 {
					break
				}
				select {
				case r := <-finished:
					t.Fatalf("task released before terminal commit: %+v %v", r.result, r.err)
				case <-deadline.C:
					t.Fatalf("terminal save was not retried: attempts=%d", attempts)
				case <-ticker.C:
				}
			}
			stored, err := c.cfg.Catalog.GetCrawlerTask(context.Background(), task.Result.DriveID, task.Result.TaskID)
			if err != nil || (stored.State != "queued" && stored.State != "running") || terminalProgress.Load() {
				t.Fatalf("unfinished commit exposed terminal state: %+v %v", stored, err)
			}
			if _, err := os.Stat(task.dir); err != nil {
				t.Fatalf("workspace lost during save retry: %v", err)
			}
			c.tasksMu.Lock()
			_, owned := c.tasks[task.Result.TaskID]
			c.tasksMu.Unlock()
			if !owned {
				t.Fatal("save retry released task ownership")
			}
			if _, err := c.Prepare(context.Background(), 1, ""); err == nil {
				t.Fatal("accepted another task before terminal commit")
			}
			if _, err := db.Exec(`DROP TRIGGER reject_terminal`); err != nil {
				t.Fatal(err)
			}
			var r outcome
			select {
			case r = <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("task did not recover after writes were restored")
			}
			if r.result.State != wantState || (canceled && !errors.Is(r.err, context.Canceled)) || (!canceled && r.err != nil) {
				t.Fatalf("retry changed the business outcome: %+v %v", r.result, r.err)
			}
			stored, err = c.cfg.Catalog.GetCrawlerTask(context.Background(), r.result.DriveID, r.result.TaskID)
			if err != nil || stored.State != wantState || !terminalProgress.Load() {
				t.Fatalf("terminal outcome missing: %+v %v", stored, err)
			}
			if _, err := os.Stat(task.dir); !os.IsNotExist(err) {
				t.Fatalf("committed workspace retained: %v", err)
			}
			c.cfg.OnProgress = nil
			if _, err := c.RunOnce(context.Background(), 1); err != nil {
				t.Fatalf("crawler remained blocked after recovery: %v", err)
			}
		})
	}
}

func TestCrawlerTerminalSaveRecoversAfterSnapshotTimeout(t *testing.T) {
	c := newRuntimeTestCrawler(t, `c=read();send(c,"page",items=[],next_cursor=None);stop()`, ProtocolV3, nil)
	task, err := c.Prepare(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	barrierHeld := make(chan struct{})
	finished := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, err := c.RunTask(context.Background(), task, func(context.Context, *Task) error {
			persistence.Lock()
			close(barrierHeld)
			return nil
		})
		finished <- err
	}()
	select {
	case <-barrierHeld:
	case err := <-finished:
		t.Fatalf("task failed before terminal persistence: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			persistence.Unlock()
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("task did not finish after snapshot released its gate")
		}
	}()
	// A snapshot may hold the gate beyond one write attempt's deadline.
	select {
	case err := <-finished:
		t.Fatalf("snapshot timeout abandoned terminal persistence: %v", err)
	case <-time.After(5200 * time.Millisecond):
	}
	if _, err := os.Stat(task.dir); err != nil {
		t.Fatalf("workspace removed while snapshot prevented saving: %v", err)
	}
	persistence.Unlock()
	locked = false
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("snapshot recovery changed the crawl outcome: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal persistence did not recover after snapshot finished")
	}
	stored, err := c.cfg.Catalog.GetCrawlerTask(context.Background(), task.Result.DriveID, task.Result.TaskID)
	if err != nil || stored.State != "completed" {
		t.Fatalf("terminal outcome missing after snapshot: %+v %v", stored, err)
	}
}
