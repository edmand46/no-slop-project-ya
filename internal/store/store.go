// Package store persists per-user tasks in SQLite. All instants are stored as
// UTC unix seconds; the caller decides in which timezone to display them.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNoTask = errors.New("task not found")

type Task struct {
	ID          int64
	UserID      int64
	Text        string
	PhotoFileID string
	DueAt       *time.Time
	RemindedAt  *time.Time
	Done        bool
	CreatedAt   time.Time
}

type Store struct {
	db *sql.DB
}

const taskColumns = "id, user_id, text, photo_file_id, due_at, reminded_at, done, created_at"

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps SQLite writes serialized and makes :memory: usable.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id       INTEGER NOT NULL,
		text          TEXT    NOT NULL,
		photo_file_id TEXT    NOT NULL DEFAULT '',
		due_at        INTEGER,
		reminded_at   INTEGER,
		done          INTEGER NOT NULL DEFAULT 0,
		created_at    INTEGER NOT NULL
	)`); err != nil {
		_ = db.Close()
		return nil, err
	}

	// Additive upgrade for todo.db files created before photos existed.
	var hasPhotoColumn int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'photo_file_id'`).Scan(&hasPhotoColumn); err != nil {
		_ = db.Close()
		return nil, err
	}
	if hasPhotoColumn == 0 {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN photo_file_id TEXT NOT NULL DEFAULT ''`); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS tasks_user_done ON tasks(user_id, done)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS tasks_due ON tasks(due_at, reminded_at)`); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Add(ctx context.Context, userID int64, text, photoFileID string, due *time.Time) (Task, error) {
	now := time.Now().UTC().Truncate(time.Second)
	var dueUnix any
	if due != nil {
		dueUnix = due.UTC().Unix()
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks(user_id, text, photo_file_id, due_at, created_at) VALUES(?, ?, ?, ?, ?)`,
		userID, text, photoFileID, dueUnix, now.Unix())
	if err != nil {
		return Task{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Task{}, err
	}

	return Task{ID: id, UserID: userID, Text: text, PhotoFileID: photoFileID, DueAt: due, CreatedAt: now}, nil
}

func (s *Store) ListActive(ctx context.Context, userID int64, offset, limit int) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE user_id = ? AND done = 0
		 ORDER BY due_at IS NULL, due_at, created_at, id
		 LIMIT ? OFFSET ?`,
		userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) CountActive(ctx context.Context, userID int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE user_id = ? AND done = 0`, userID).Scan(&count)
	return count, err
}

func (s *Store) Get(ctx context.Context, userID, id int64) (Task, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = ? AND user_id = ?`, id, userID)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNoTask
	}
	return task, err
}

func (s *Store) Done(ctx context.Context, userID, id int64) (Task, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET done = 1 WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return Task{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Task{}, err
	}
	if n == 0 {
		return Task{}, ErrNoTask
	}
	return s.Get(ctx, userID, id)
}

func (s *Store) Delete(ctx context.Context, userID, id int64) (Task, error) {
	task, err := s.Get(ctx, userID, id)
	if err != nil {
		return Task{}, err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM tasks WHERE id = ? AND user_id = ?`, id, userID); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Store) DueReminders(ctx context.Context, now time.Time) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE done = 0 AND due_at IS NOT NULL AND reminded_at IS NULL AND due_at <= ?
		 ORDER BY due_at`,
		now.UTC().Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) MarkReminded(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET reminded_at = ? WHERE id = ?`, at.UTC().Unix(), id)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(sc scanner) (Task, error) {
	var (
		task                   Task
		due, reminded, created sql.NullInt64
		done                   int
	)
	if err := sc.Scan(&task.ID, &task.UserID, &task.Text, &task.PhotoFileID, &due, &reminded, &done, &created); err != nil {
		return Task{}, err
	}
	if due.Valid {
		v := time.Unix(due.Int64, 0).UTC()
		task.DueAt = &v
	}
	if reminded.Valid {
		v := time.Unix(reminded.Int64, 0).UTC()
		task.RemindedAt = &v
	}
	task.Done = done != 0
	task.CreatedAt = time.Unix(created.Int64, 0).UTC()
	return task, nil
}
