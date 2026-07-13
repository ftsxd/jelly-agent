package schedule

import (
	"database/sql"
	"fmt"
	"github.com/jelly-agent/jelly-agent/internal/session"
	"time"
)

type Run struct {
	ID         int64     `json:"id"`
	Task       string    `json:"task"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Status     string    `json:"status"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func Record(task string, started time.Time, status, output, message string) error {
	db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS schedule_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, task TEXT NOT NULL, started_at DATETIME NOT NULL, finished_at DATETIME NOT NULL, status TEXT NOT NULL, output TEXT, error TEXT)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO schedule_runs(task,started_at,finished_at,status,output,error) VALUES(?,?,?,?,?,?)`, task, started, time.Now(), status, output, message)
	return err
}
func List(task string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	db, err := open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS schedule_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, task TEXT NOT NULL, started_at DATETIME NOT NULL, finished_at DATETIME NOT NULL, status TEXT NOT NULL, output TEXT, error TEXT)`); err != nil {
		return nil, err
	}
	q := `SELECT id,task,started_at,finished_at,status,output,error FROM schedule_runs`
	args := []any{}
	if task != "" {
		q += ` WHERE task=?`
		args = append(args, task)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var x Run
		if err := rows.Scan(&x.ID, &x.Task, &x.StartedAt, &x.FinishedAt, &x.Status, &x.Output, &x.Error); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func open() (*sql.DB, error) {
	p, err := session.DefaultDBPath()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		return nil, fmt.Errorf("open schedule db: %w", err)
	}
	return db, nil
}
