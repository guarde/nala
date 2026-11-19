package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/lib/pq"
)

type Workflow struct {
	ID        string          `json:"id"`
	OrgID     string          `json:"org_id"`
	Name      string          `json:"name"`
	Graph     json.RawMessage `json:"graph,omitempty"`
	CreatedBy string          `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Nodes     int             `json:"nodes"`
}

const workflowCols = `id, org_id, name, graph, created_by, created_at, updated_at, jsonb_array_length(graph->'nodes')`

func scanWorkflow(row interface{ Scan(...any) error }) (Workflow, error) {
	var w Workflow
	var g []byte
	err := row.Scan(&w.ID, &w.OrgID, &w.Name, &g, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt, &w.Nodes)
	w.Graph = g
	return w, notFound(err)
}

func (s *Store) CreateWorkflow(ctx context.Context, orgID, name string, graph []byte, createdBy string) (Workflow, error) {
	return scanWorkflow(s.DB.QueryRowContext(ctx,
		`INSERT INTO workflows (org_id, name, graph, created_by) VALUES ($1, $2, $3, $4) RETURNING `+workflowCols,
		orgID, name, graph, createdBy))
}

func (s *Store) UpdateWorkflow(ctx context.Context, orgID, id, name string, graph []byte) (Workflow, error) {
	return scanWorkflow(s.DB.QueryRowContext(ctx,
		`UPDATE workflows SET name = $3, graph = $4, updated_at = now() WHERE org_id = $1 AND id = $2 RETURNING `+workflowCols,
		orgID, id, name, graph))
}

func (s *Store) GetWorkflow(ctx context.Context, orgID, id string) (Workflow, error) {
	return scanWorkflow(s.DB.QueryRowContext(ctx, `SELECT `+workflowCols+` FROM workflows WHERE org_id = $1 AND id = $2`, orgID, id))
}

func (s *Store) DeleteWorkflow(ctx context.Context, orgID, id string) error {
	return affected(s.DB.ExecContext(ctx, `DELETE FROM workflows WHERE org_id = $1 AND id = $2`, orgID, id))
}

func (s *Store) ListWorkflows(ctx context.Context, orgID string) ([]Workflow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+workflowCols+` FROM workflows WHERE org_id = $1 ORDER BY updated_at DESC LIMIT 200`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workflow{}
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		w.Graph = nil
		out = append(out, w)
	}
	return out, rows.Err()
}

type Run struct {
	ID         string          `json:"id"`
	OrgID      string          `json:"org_id"`
	WorkflowID *string         `json:"workflow_id"`
	Name       string          `json:"name"`
	Graph      json.RawMessage `json:"graph,omitempty"`
	Status     string          `json:"status"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at"`
}

const runCols = `id, org_id, workflow_id, name, graph, status, created_by, created_at, finished_at`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var g []byte
	err := row.Scan(&r.ID, &r.OrgID, &r.WorkflowID, &r.Name, &g, &r.Status, &r.CreatedBy, &r.CreatedAt, &r.FinishedAt)
	r.Graph = g
	return r, notFound(err)
}

type RunEdge struct {
	From, To, On string
}

func (s *Store) StartRun(ctx context.Context, orgID string, workflowID *string, name string, graph []byte, createdBy string, nodes []NewJob, edges []RunEdge) (Run, error) {
	var run Run
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		run, err = scanRun(tx.QueryRowContext(ctx,
			`INSERT INTO workflow_runs (org_id, workflow_id, name, graph, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING `+runCols,
			orgID, workflowID, name, graph, createdBy))
		if err != nil {
			return err
		}
		hasParent := map[string]bool{}
		for _, e := range edges {
			hasParent[e.To] = true
		}
		ids := map[string]string{}
		for _, n := range nodes {
			env, err := json.Marshal(n.Env)
			if err != nil {
				return err
			}
			if n.Env == nil {
				env = []byte("{}")
			}
			if n.Command == nil {
				n.Command = []string{}
			}
			status := StatusPending
			if hasParent[n.NodeID] {
				status = StatusBlocked
			}
			var id string
			err = tx.QueryRowContext(ctx,
				`INSERT INTO jobs (org_id, queue, image, command, env, priority, max_attempts, timeout_seconds, created_by, name, network, run_id, node_id, status)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id`,
				orgID, n.Queue, n.Image, pq.Array(n.Command), env, n.Priority, n.MaxAttempts, n.TimeoutSeconds,
				createdBy, n.Name, n.Network, run.ID, n.NodeID, status).Scan(&id)
			if err != nil {
				return err
			}
			ids[n.NodeID] = id
		}
		for _, e := range edges {
			if _, err := tx.ExecContext(ctx, `INSERT INTO job_deps (job_id, parent_id, condition) VALUES ($1, $2, $3)`, ids[e.To], ids[e.From], e.On); err != nil {
				return err
			}
		}
		return nil
	})
	return run, err
}

func (s *Store) GetRun(ctx context.Context, orgID, id string) (Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runCols+` FROM workflow_runs WHERE org_id = $1 AND id = $2`, orgID, id))
}

func (s *Store) ListRuns(ctx context.Context, orgID, workflowID string, limit int) ([]Run, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+runCols+` FROM workflow_runs
		 WHERE org_id = $1 AND ($2 = '' OR workflow_id = NULLIF($2, '')::uuid) ORDER BY created_at DESC LIMIT $3`, orgID, workflowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		r.Graph = nil
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CancelRun(ctx context.Context, orgID, id string) error {
	run, err := s.GetRun(ctx, orgID, id)
	if err != nil {
		return err
	}
	if run.Status != "running" {
		return ErrConflict
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE jobs SET
		   status = CASE WHEN status = 'running' THEN status ELSE 'cancelled' END,
		   finished_at = CASE WHEN status = 'running' THEN finished_at ELSE now() END,
		   cancel_requested = (status = 'running')
		 WHERE run_id = $1 AND status IN ('pending', 'blocked', 'running')`, id); err != nil {
		return err
	}
	return s.AdvanceRun(ctx, id)
}

func (s *Store) advance(ctx context.Context, runID *string) error {
	if runID == nil {
		return nil
	}
	return s.AdvanceRun(ctx, *runID)
}

func (s *Store) AdvanceRun(ctx context.Context, runID string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, runID); err != nil {
			return err
		}
		for {
			res, err := tx.ExecContext(ctx,
				`WITH ready AS (
				   SELECT c.id, bool_and(CASE d.condition
				       WHEN 'success' THEN p.status = 'succeeded'
				       WHEN 'failure' THEN p.status = 'dead'
				       ELSE p.status IN ('succeeded', 'dead', 'cancelled') END) AS go
				   FROM jobs c
				   JOIN job_deps d ON d.job_id = c.id
				   JOIN jobs p ON p.id = d.parent_id
				   WHERE c.run_id = $1 AND c.status = 'blocked'
				   GROUP BY c.id
				   HAVING bool_and(p.status IN ('succeeded', 'dead', 'cancelled', 'skipped')))
				 UPDATE jobs j SET
				   status = CASE WHEN r.go THEN 'pending' ELSE 'skipped' END,
				   run_at = now(),
				   finished_at = CASE WHEN r.go THEN NULL ELSE now() END
				 FROM ready r WHERE j.id = r.id`, runID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				break
			}
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE workflow_runs r SET finished_at = now(), status = CASE
			     WHEN EXISTS (SELECT 1 FROM jobs WHERE run_id = r.id AND status = 'dead') THEN 'failed'
			     WHEN EXISTS (SELECT 1 FROM jobs WHERE run_id = r.id AND status = 'cancelled') THEN 'cancelled'
			     ELSE 'succeeded' END
			 WHERE r.id = $1 AND r.status = 'running'
			   AND NOT EXISTS (SELECT 1 FROM jobs WHERE run_id = r.id AND status IN ('pending', 'blocked', 'running'))`, runID)
		return err
	})
}

func (s *Store) AdvanceStalledRuns(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT r.id FROM workflow_runs r
		 WHERE r.status = 'running'
		   AND NOT EXISTS (SELECT 1 FROM jobs WHERE run_id = r.id AND status IN ('pending', 'running'))
		 LIMIT 100`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.AdvanceRun(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
