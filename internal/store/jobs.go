package store

/// AHHHHH THE J WORD...

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	StatusPending   = "pending"
	StatusBlocked   = "blocked"
	StatusSkipped   = "skipped"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusDead      = "dead"
	StatusCancelled = "cancelled"
)

type Job struct {
	ID              string            `json:"id"`
	OrgID           string            `json:"org_id"`
	Name            string            `json:"name"`
	Network         string            `json:"network"`
	RunID           *string           `json:"run_id"`
	NodeID          *string           `json:"node_id"`
	Queue           string            `json:"queue"`
	Image           string            `json:"image"`
	Command         []string          `json:"command"`
	Env             map[string]string `json:"-"`
	EnvKeys         []string          `json:"env_keys"`
	Status          string            `json:"status"`
	Priority        int               `json:"priority"`
	Attempts        int               `json:"attempts"`
	MaxAttempts     int               `json:"max_attempts"`
	TimeoutSeconds  int               `json:"timeout_seconds"`
	RunAt           time.Time         `json:"run_at"`
	LockedBy        *string           `json:"locked_by"`
	CancelRequested bool              `json:"cancel_requested"`
	LastError       *string           `json:"last_error"`
	ExitCode        *int              `json:"exit_code"`
	IdempotencyKey  *string           `json:"idempotency_key"`
	CreatedBy       string            `json:"created_by"`
	CreatedAt       time.Time         `json:"created_at"`
	StartedAt       *time.Time        `json:"started_at"`
	FinishedAt      *time.Time        `json:"finished_at"`
}

const jobCols = `id, org_id, queue, image, command, env, status, priority, attempts, max_attempts,
    timeout_seconds, run_at, locked_by, cancel_requested, last_error, exit_code, idempotency_key,
    created_by, created_at, started_at, finished_at, name, network, run_id, node_id`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var env []byte
	err := row.Scan(&j.ID, &j.OrgID, &j.Queue, &j.Image, pq.Array(&j.Command), &env, &j.Status, &j.Priority,
		&j.Attempts, &j.MaxAttempts, &j.TimeoutSeconds, &j.RunAt, &j.LockedBy, &j.CancelRequested,
		&j.LastError, &j.ExitCode, &j.IdempotencyKey, &j.CreatedBy, &j.CreatedAt, &j.StartedAt, &j.FinishedAt,
		&j.Name, &j.Network, &j.RunID, &j.NodeID)
	if err != nil {
		return j, notFound(err)
	}
	if err := json.Unmarshal(env, &j.Env); err != nil {
		return j, err
	}
	if j.Command == nil {
		j.Command = []string{}
	}
	j.EnvKeys = make([]string, 0, len(j.Env))
	for k := range j.Env {
		j.EnvKeys = append(j.EnvKeys, k)
	}
	sort.Strings(j.EnvKeys)
	return j, nil
}

type NewJob struct {
	OrgID          string
	Name           string
	Network        string
	NodeID         string
	Queue          string
	Image          string
	Command        []string
	Env            map[string]string
	Priority       int
	MaxAttempts    int
	TimeoutSeconds int
	RunAt          *time.Time
	IdempotencyKey *string
	CreatedBy      string
}

func (s *Store) Enqueue(ctx context.Context, n NewJob) (job Job, created bool, err error) {
	env, err := json.Marshal(n.Env)
	if err != nil {
		return job, false, err
	}
	if n.Env == nil {
		env = []byte("{}")
	}
	if n.Network == "" {
		n.Network = "none"
	}
	if n.Command == nil {
		n.Command = []string{}
	}
	job, err = scanJob(s.DB.QueryRowContext(ctx,
		`INSERT INTO jobs (org_id, queue, image, command, env, priority, max_attempts, timeout_seconds, run_at, idempotency_key, created_by, name, network)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, now()), $10, $11, $12, $13)
         ON CONFLICT (org_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
         RETURNING `+jobCols,
		n.OrgID, n.Queue, n.Image, pq.Array(n.Command), env, n.Priority, n.MaxAttempts, n.TimeoutSeconds, n.RunAt, n.IdempotencyKey, n.CreatedBy, n.Name, n.Network))
	if err == ErrNotFound && n.IdempotencyKey != nil {
		job, err = scanJob(s.DB.QueryRowContext(ctx,
			`SELECT `+jobCols+` FROM jobs WHERE org_id = $1 AND idempotency_key = $2`, n.OrgID, *n.IdempotencyKey))
		return job, false, err
	}
	return job, err == nil, err
}

func (s *Store) GetJob(ctx context.Context, orgID, id string) (Job, error) {
	return scanJob(s.DB.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE org_id = $1 AND id = $2`, orgID, id))
}

type JobFilter struct {
	OrgID  *string
	RunID  string
	Status string
	Queue  string
	Limit  int
	Offset int
}

func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.OrgID != nil {
		add("org_id = $%d", *f.OrgID)
	}
	if f.RunID != "" {
		add("run_id = $%d", f.RunID)
	}
	if f.Status != "" {
		add("status = $%d", f.Status)
	}
	if f.Queue != "" {
		add("queue = $%d", f.Queue)
	}
	q := `SELECT ` + jobCols + ` FROM jobs`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	args = append(args, f.Limit, f.Offset)
	q += fmt.Sprintf(` ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) one(ctx context.Context, orgID, id, query string, args ...any) (Job, error) {
	j, err := scanJob(s.DB.QueryRowContext(ctx, query, args...))
	if err == ErrNotFound {
		if _, gerr := s.GetJob(ctx, orgID, id); gerr == nil {
			return j, ErrConflict
		}
	}
	return j, err
}

func (s *Store) CancelJob(ctx context.Context, orgID, id string) (Job, error) {
	j, err := s.one(ctx, orgID, id,
		`UPDATE jobs SET
           status = CASE WHEN status = 'running' THEN status ELSE 'cancelled' END,
           finished_at = CASE WHEN status = 'running' THEN finished_at ELSE now() END,
           cancel_requested = (status = 'running')
         WHERE org_id = $1 AND id = $2 AND status IN ('pending', 'blocked', 'running')
         RETURNING `+jobCols, orgID, id)
	if err != nil {
		return j, err
	}
	return j, s.advance(ctx, j.RunID)
}

func (s *Store) RetryJob(ctx context.Context, orgID, id string) (Job, error) {
	j, err := s.one(ctx, orgID, id,
		`UPDATE jobs SET status = 'pending', attempts = 0, run_at = now(), last_error = NULL, exit_code = NULL,
           cancel_requested = false, started_at = NULL, finished_at = NULL
         WHERE org_id = $1 AND id = $2 AND status IN ('dead', 'cancelled')
         RETURNING `+jobCols, orgID, id)
	if err != nil || j.RunID == nil {
		return j, err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET status = 'running', finished_at = NULL WHERE id = $1`, *j.RunID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'blocked', finished_at = NULL WHERE run_id = $1 AND status = 'skipped'`, *j.RunID)
		return err
	})
	if err != nil {
		return j, err
	}
	return j, s.advance(ctx, j.RunID)
}

func (s *Store) DeleteJob(ctx context.Context, orgID, id string) error {
	_, err := s.one(ctx, orgID, id,
		`DELETE FROM jobs WHERE org_id = $1 AND id = $2 AND status <> 'running' AND run_id IS NULL RETURNING `+jobCols, orgID, id)
	return err
}

func (s *Store) CountQueued(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE org_id = $1 AND status IN ('pending', 'blocked')`, orgID).Scan(&n)
	return n, err
}

func (s *Store) Claim(ctx context.Context, workerID string, lease time.Duration) (*Job, error) {
	var claimed *Job
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		full := []string{}
		for range 5 {
			var id, orgID string
			err := tx.QueryRowContext(ctx,
				`SELECT j.id, j.org_id FROM jobs j
                 WHERE j.status = 'pending' AND j.run_at <= now()
                   AND j.org_id <> ALL($1::uuid[])
                   AND NOT EXISTS (SELECT 1 FROM queues q WHERE q.org_id = j.org_id AND q.name = j.queue AND q.paused)
                   AND (SELECT count(*) FROM jobs r WHERE r.org_id = j.org_id AND r.status = 'running')
                       < (SELECT o.max_running FROM orgs o WHERE o.id = j.org_id)
                 ORDER BY j.priority DESC, j.run_at, j.created_at
                 FOR UPDATE OF j SKIP LOCKED
                 LIMIT 1`, pq.Array(full)).Scan(&id, &orgID)
			if err == sql.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			var limit, running int
			if err := tx.QueryRowContext(ctx, `SELECT max_running FROM orgs WHERE id = $1 FOR NO KEY UPDATE`, orgID).Scan(&limit); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE org_id = $1 AND status = 'running'`, orgID).Scan(&running); err != nil {
				return err
			}
			if running >= limit {
				full = append(full, orgID)
				continue
			}
			j, err := scanJob(tx.QueryRowContext(ctx,
				`UPDATE jobs SET status = 'running', attempts = attempts + 1, locked_by = $2,
                   locked_until = now() + make_interval(secs => $3), started_at = now(),
                   cancel_requested = false, exit_code = NULL, proxy_token_hash = NULL
                 WHERE id = $1 RETURNING `+jobCols, id, workerID, lease.Seconds()))
			if err != nil {
				return err
			}
			claimed = &j
			return nil
		}
		return nil
	})
	return claimed, err
}

func (s *Store) Heartbeat(ctx context.Context, workerID string, lease time.Duration) (map[string]bool, error) {
	if _, err := s.DB.ExecContext(ctx, `UPDATE workers SET last_seen_at = now() WHERE id = $1`, workerID); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx,
		`UPDATE jobs SET locked_until = now() + make_interval(secs => $2)
         WHERE locked_by = $1 AND status = 'running' RETURNING id, cancel_requested`, workerID, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := map[string]bool{}
	for rows.Next() {
		var id string
		var cancel bool
		if err := rows.Scan(&id, &cancel); err != nil {
			return nil, err
		}
		held[id] = cancel
	}
	return held, rows.Err()
}

const ownGuard = ` WHERE id = $1 AND locked_by = $2 AND status = 'running' AND attempts = $3`

func (s *Store) Succeed(ctx context.Context, j *Job, workerID string) error {
	err := affected(s.DB.ExecContext(ctx,
		`UPDATE jobs SET status = 'succeeded', exit_code = 0, last_error = NULL, finished_at = now(),
           locked_by = NULL, locked_until = NULL, proxy_token_hash = NULL`+ownGuard, j.ID, workerID, j.Attempts))
	if err != nil {
		return err
	}
	return s.advance(ctx, j.RunID)
}

func (s *Store) Fail(ctx context.Context, j *Job, workerID, reason string, exitCode *int, backoff time.Duration) (status string, err error) {
	err = s.DB.QueryRowContext(ctx,
		`UPDATE jobs SET
           status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END,
           finished_at = CASE WHEN attempts >= max_attempts THEN now() END,
           run_at = now() + make_interval(secs => $6),
           last_error = $4, exit_code = $5, locked_by = NULL, locked_until = NULL, proxy_token_hash = NULL`+ownGuard+` RETURNING status`,
		j.ID, workerID, j.Attempts, reason, exitCode, backoff.Seconds()).Scan(&status)
	if err != nil {
		return status, notFound(err)
	}
	if status == StatusDead {
		err = s.advance(ctx, j.RunID)
	}
	return status, err
}

func (s *Store) MarkCancelled(ctx context.Context, j *Job, workerID string) error {
	err := affected(s.DB.ExecContext(ctx,
		`UPDATE jobs SET status = 'cancelled', last_error = 'cancelled by request', finished_at = now(),
           locked_by = NULL, locked_until = NULL, proxy_token_hash = NULL`+ownGuard, j.ID, workerID, j.Attempts))
	if err != nil {
		return err
	}
	return s.advance(ctx, j.RunID)
}

func (s *Store) Release(ctx context.Context, j *Job, workerID string) error {
	return affected(s.DB.ExecContext(ctx,
		`UPDATE jobs SET status = 'pending', attempts = attempts - 1, run_at = now(), started_at = NULL,
           locked_by = NULL, locked_until = NULL, proxy_token_hash = NULL`+ownGuard, j.ID, workerID, j.Attempts))
}

func (s *Store) SetProxyToken(ctx context.Context, j *Job, workerID string, hash []byte) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE jobs SET proxy_token_hash = $4`+ownGuard, j.ID, workerID, j.Attempts, hash))
}

func (s *Store) Reap(ctx context.Context) (int, error) {
	rows, err := s.DB.QueryContext(ctx,
		`WITH reaped AS (
           UPDATE jobs SET
             status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END,
             finished_at = CASE WHEN attempts >= max_attempts THEN now() END,
             run_at = now() + make_interval(secs => LEAST(3600, 5 * power(2, attempts - 1))),
             last_error = 'worker lost: lease expired', locked_by = NULL, locked_until = NULL, proxy_token_hash = NULL
           WHERE id IN (SELECT id FROM jobs WHERE status = 'running' AND locked_until < now() FOR UPDATE SKIP LOCKED)
           RETURNING id, attempts, status, run_id),
         logged AS (
           INSERT INTO job_logs (job_id, attempt, stream, line)
           SELECT id, attempts, 'system',
             CASE WHEN status = 'dead' THEN 'worker lost (lease expired); no attempts left, job is dead'
                  ELSE 'worker lost (lease expired); job requeued' END
           FROM reaped)
         SELECT run_id FROM reaped`)
	if err != nil {
		return 0, err
	}
	n := 0
	runs := map[string]bool{}
	for rows.Next() {
		var runID *string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return n, err
		}
		n++
		if runID != nil {
			runs[*runID] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return n, err
	}
	for id := range runs {
		if err := s.AdvanceRun(ctx, id); err != nil {
			return n, err
		}
	}
	return n, nil
}

type LogLine struct {
	ID      int64     `json:"id"`
	Attempt int       `json:"attempt"`
	Stream  string    `json:"stream"`
	Line    string    `json:"line"`
	TS      time.Time `json:"ts"`
}

func (s *Store) AppendLogs(ctx context.Context, jobID string, attempt int, lines []LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	streams := make([]string, len(lines))
	texts := make([]string, len(lines))
	times := make([]string, len(lines))
	for i, l := range lines {
		streams[i], texts[i], times[i] = l.Stream, l.Line, l.TS.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO job_logs (job_id, attempt, stream, line, ts)
         SELECT $1, $2, t.stream, t.line, t.ts::timestamptz
         FROM unnest($3::text[], $4::text[], $5::text[]) WITH ORDINALITY AS t(stream, line, ts, ord)
         ORDER BY t.ord`,
		jobID, attempt, pq.Array(streams), pq.Array(texts), pq.Array(times))
	return err
}

func (s *Store) JobLogs(ctx context.Context, orgID, jobID string, afterID int64, limit int) ([]LogLine, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT l.id, l.attempt, l.stream, l.line, l.ts FROM job_logs l JOIN jobs j ON j.id = l.job_id
         WHERE j.org_id = $1 AND l.job_id = $2 AND l.id > $3 ORDER BY l.id LIMIT $4`, orgID, jobID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogLine{}
	for rows.Next() {
		var l LogLine
		if err := rows.Scan(&l.ID, &l.Attempt, &l.Stream, &l.Line, &l.TS); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

type QueueInfo struct {
	Name    string `json:"name"`
	Paused  bool   `json:"paused"`
	Pending int    `json:"pending"`
	Running int    `json:"running"`
}

func (s *Store) ListQueues(ctx context.Context, orgID string) ([]QueueInfo, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT name, bool_or(paused), sum(pending)::int, sum(running)::int FROM (
           SELECT name, paused, 0 AS pending, 0 AS running FROM queues WHERE org_id = $1
           UNION ALL
           SELECT queue, false, count(*) FILTER (WHERE status = 'pending'), count(*) FILTER (WHERE status = 'running')
           FROM jobs WHERE org_id = $1 GROUP BY queue) t
         GROUP BY name ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueueInfo{}
	for rows.Next() {
		var q QueueInfo
		if err := rows.Scan(&q.Name, &q.Paused, &q.Pending, &q.Running); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) SetQueuePaused(ctx context.Context, orgID, name string, paused bool) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO queues (org_id, name, paused) VALUES ($1, $2, $3)
         ON CONFLICT (org_id, name) DO UPDATE SET paused = EXCLUDED.paused`, orgID, name, paused)
	return err
}

type WorkerInfo struct {
	ID          string     `json:"id"`
	Hostname    string     `json:"hostname"`
	Concurrency int        `json:"concurrency"`
	StartedAt   time.Time  `json:"started_at"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	StoppedAt   *time.Time `json:"stopped_at"`
	Running     int        `json:"running_jobs"`
}

func (s *Store) RegisterWorker(ctx context.Context, id, hostname string, concurrency int) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO workers (id, hostname, concurrency) VALUES ($1, $2, $3)`, id, hostname, concurrency)
	return err
}

func (s *Store) StopWorker(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE workers SET stopped_at = now(), last_seen_at = now() WHERE id = $1`, id)
	return err
}

func (s *Store) ListWorkers(ctx context.Context, limit int) ([]WorkerInfo, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT w.id, w.hostname, w.concurrency, w.started_at, w.last_seen_at, w.stopped_at,
           (SELECT count(*) FROM jobs j WHERE j.locked_by = w.id AND j.status = 'running')
         FROM workers w ORDER BY w.started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerInfo{}
	for rows.Next() {
		var w WorkerInfo
		if err := rows.Scan(&w.ID, &w.Hostname, &w.Concurrency, &w.StartedAt, &w.LastSeenAt, &w.StoppedAt, &w.Running); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) OrgStats(ctx context.Context, orgID string) (map[string]int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT status, count(*) FROM jobs WHERE org_id = $1 GROUP BY status`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

type Stats struct {
	Users       int            `json:"users"`
	Orgs        int            `json:"orgs"`
	LiveWorkers int            `json:"live_workers"`
	Jobs        map[string]int `json:"jobs_by_status"`
}

func (s *Store) Stats(ctx context.Context, liveWindow time.Duration) (Stats, error) {
	st := Stats{Jobs: map[string]int{}}
	err := s.DB.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM orgs),
           (SELECT count(*) FROM workers WHERE stopped_at IS NULL AND last_seen_at > now() - make_interval(secs => $1))`,
		liveWindow.Seconds()).Scan(&st.Users, &st.Orgs, &st.LiveWorkers)
	if err != nil {
		return st, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT status, count(*) FROM jobs GROUP BY status`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return st, err
		}
		st.Jobs[status] = n
	}
	return st, rows.Err()
}
