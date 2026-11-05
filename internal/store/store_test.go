package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error: %v", err)
		}
	})
	return s
}

func TestAddGetAndIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	due := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	task, err := s.Add(ctx, 1, "купить молоко", "", &due)
	if err != nil {
		t.Fatalf("Add() error: %v", err)
	}
	if task.ID == 0 {
		t.Fatal("Add() returned zero ID")
	}

	got, err := s.Get(ctx, 1, task.ID)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.Text != "купить молоко" || got.DueAt == nil || !got.DueAt.Equal(due) || got.Done {
		t.Fatalf("Get() = %+v", got)
	}

	if _, err := s.Get(ctx, 2, task.ID); !errors.Is(err, ErrNoTask) {
		t.Fatalf("Get() for another user = %v, want ErrNoTask", err)
	}
	if _, err := s.Done(ctx, 2, task.ID); !errors.Is(err, ErrNoTask) {
		t.Fatalf("Done() for another user = %v, want ErrNoTask", err)
	}
	if _, err := s.Delete(ctx, 2, task.ID); !errors.Is(err, ErrNoTask) {
		t.Fatalf("Delete() for another user = %v, want ErrNoTask", err)
	}
}

func TestListOrderAndPagination(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	dueSoon := now.Add(time.Hour)
	dueLater := now.Add(2 * time.Hour)

	a := mustAdd(t, s, 1, "без срока 1", nil)
	b := mustAdd(t, s, 1, "со сроком позже", &dueLater)
	c := mustAdd(t, s, 1, "со сроком раньше", &dueSoon)
	d := mustAdd(t, s, 1, "без срока 2", nil)

	tasks, err := s.ListActive(ctx, 1, 0, 10)
	if err != nil {
		t.Fatalf("ListActive() error: %v", err)
	}
	want := []int64{c.ID, b.ID, a.ID, d.ID}
	if len(tasks) != len(want) {
		t.Fatalf("ListActive() returned %d tasks, want %d", len(tasks), len(want))
	}
	for i, id := range want {
		if tasks[i].ID != id {
			t.Fatalf("ListActive()[%d].ID = %d, want %d", i, tasks[i].ID, id)
		}
	}

	page, err := s.ListActive(ctx, 1, 2, 2)
	if err != nil {
		t.Fatalf("ListActive() page error: %v", err)
	}
	if len(page) != 2 || page[0].ID != a.ID || page[1].ID != d.ID {
		t.Fatalf("ListActive() second page = %+v", page)
	}

	count, err := s.CountActive(ctx, 1)
	if err != nil {
		t.Fatalf("CountActive() error: %v", err)
	}
	if count != 4 {
		t.Fatalf("CountActive() = %d, want 4", count)
	}
}

func TestDoneAndDelete(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	task := mustAdd(t, s, 1, "позвонить маме", nil)

	done, err := s.Done(ctx, 1, task.ID)
	if err != nil {
		t.Fatalf("Done() error: %v", err)
	}
	if !done.Done {
		t.Fatal("Done() did not mark task done")
	}

	active, err := s.ListActive(ctx, 1, 0, 10)
	if err != nil {
		t.Fatalf("ListActive() error: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive() after Done() = %+v, want empty", active)
	}
	if count, _ := s.CountActive(ctx, 1); count != 0 {
		t.Fatalf("CountActive() after Done() = %d, want 0", count)
	}

	deleted, err := s.Delete(ctx, 1, task.ID)
	if err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if deleted.Text != "позвонить маме" {
		t.Fatalf("Delete() text = %q", deleted.Text)
	}
	if _, err := s.Delete(ctx, 1, task.ID); !errors.Is(err, ErrNoTask) {
		t.Fatalf("second Delete() = %v, want ErrNoTask", err)
	}
}

func TestDueReminders(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)

	duePast := mustAdd(t, s, 1, "просрочена", &past)
	mustAdd(t, s, 1, "в будущем", &future)
	mustAdd(t, s, 1, "без срока", nil)
	donePast := mustAdd(t, s, 1, "выполнена", &past)
	if _, err := s.Done(ctx, 1, donePast.ID); err != nil {
		t.Fatalf("Done() error: %v", err)
	}

	reminders, err := s.DueReminders(ctx, now)
	if err != nil {
		t.Fatalf("DueReminders() error: %v", err)
	}
	if len(reminders) != 1 || reminders[0].ID != duePast.ID {
		t.Fatalf("DueReminders() = %+v, want only %d", reminders, duePast.ID)
	}

	if err := s.MarkReminded(ctx, duePast.ID, now); err != nil {
		t.Fatalf("MarkReminded() error: %v", err)
	}
	reminders, err = s.DueReminders(ctx, now)
	if err != nil {
		t.Fatalf("DueReminders() after MarkReminded() error: %v", err)
	}
	if len(reminders) != 0 {
		t.Fatalf("DueReminders() after MarkReminded() = %+v, want empty", reminders)
	}
}

func TestPhotoFileID(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(time.Hour)
	task, err := s.Add(ctx, 1, "чек из магазина", "photo-file-id", &due)
	if err != nil {
		t.Fatalf("Add() error: %v", err)
	}

	got, err := s.Get(ctx, 1, task.ID)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.PhotoFileID != "photo-file-id" {
		t.Fatalf("Get().PhotoFileID = %q, want %q", got.PhotoFileID, "photo-file-id")
	}

	list, err := s.ListActive(ctx, 1, 0, 10)
	if err != nil {
		t.Fatalf("ListActive() error: %v", err)
	}
	if len(list) != 1 || list[0].PhotoFileID != "photo-file-id" {
		t.Fatalf("ListActive() = %+v, want photo-file-id", list)
	}

	reminders, err := s.DueReminders(ctx, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("DueReminders() error: %v", err)
	}
	if len(reminders) != 1 || reminders[0].PhotoFileID != "photo-file-id" {
		t.Fatalf("DueReminders() = %+v, want photo-file-id", reminders)
	}

	plain, err := s.Add(ctx, 1, "без фото", "", nil)
	if err != nil {
		t.Fatalf("Add() without photo error: %v", err)
	}
	if plain.PhotoFileID != "" {
		t.Fatalf("Add() without photo PhotoFileID = %q, want empty", plain.PhotoFileID)
	}
}

func TestOpenUpgradesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE tasks (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id     INTEGER NOT NULL,
		text        TEXT    NOT NULL,
		due_at      INTEGER,
		reminded_at INTEGER,
		done        INTEGER NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	res, err := raw.Exec(`INSERT INTO tasks(user_id, text, created_at) VALUES(1, 'старая задача', 1700000000)`)
	if err != nil {
		t.Fatalf("insert legacy task: %v", err)
	}
	legacyID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() legacy db error: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error: %v", err)
		}
	})

	legacy, err := s.Get(context.Background(), 1, legacyID)
	if err != nil {
		t.Fatalf("Get() legacy task error: %v", err)
	}
	if legacy.Text != "старая задача" || legacy.PhotoFileID != "" {
		t.Fatalf("legacy task = %+v", legacy)
	}

	added, err := s.Add(context.Background(), 1, "с фото", "photo-file-id", nil)
	if err != nil {
		t.Fatalf("Add() after upgrade error: %v", err)
	}
	got, err := s.Get(context.Background(), 1, added.ID)
	if err != nil {
		t.Fatalf("Get() after upgrade error: %v", err)
	}
	if got.PhotoFileID != "photo-file-id" {
		t.Fatalf("upgraded PhotoFileID = %q, want %q", got.PhotoFileID, "photo-file-id")
	}
}

func mustAdd(t *testing.T, s *Store, userID int64, text string, due *time.Time) Task {
	t.Helper()
	task, err := s.Add(context.Background(), userID, text, "", due)
	if err != nil {
		t.Fatalf("Add(%q) error: %v", text, err)
	}
	return task
}
