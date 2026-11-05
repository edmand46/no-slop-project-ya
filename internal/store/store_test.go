package store

import (
	"context"
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
	task, err := s.Add(ctx, 1, "купить молоко", &due)
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

func mustAdd(t *testing.T, s *Store, userID int64, text string, due *time.Time) Task {
	t.Helper()
	task, err := s.Add(context.Background(), userID, text, due)
	if err != nil {
		t.Fatalf("Add(%q) error: %v", text, err)
	}
	return task
}
