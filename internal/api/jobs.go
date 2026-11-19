package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"guarde.sh/nala/internal/perms"
	"guarde.sh/nala/internal/sandbox"
	"guarde.sh/nala/internal/store"
)

var queueRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

type jobSpec struct {
	Name           string            `json:"name"`
	Queue          string            `json:"queue"`
	Image          string            `json:"image"`
	Command        []string          `json:"command"`
	Env            map[string]string `json:"env"`
	Priority       int               `json:"priority"`
	MaxAttempts    *int              `json:"max_attempts"`
	TimeoutSeconds *int              `json:"timeout_seconds"`
	Network        string            `json:"network"`
}

type enqueueBody struct {
	jobSpec
	RunAt          *time.Time `json:"run_at"`
	IdempotencyKey *string    `json:"idempotency_key"`
}

func (in *jobSpec) checkShape() error {
	if in.Queue == "" {
		in.Queue = "default"
	}
	if in.Network == "" {
		in.Network = "none"
	}
	if in.Command == nil {
		in.Command = []string{}
	}
	if in.Env == nil {
		in.Env = map[string]string{}
	}
	if utf8.RuneCountInString(in.Name) > 100 || strings.ContainsFunc(in.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errBadRequest("name must be at most 100 printable characters")
	}
	if !queueRe.MatchString(in.Queue) {
		return errBadRequest("queue must match %s", queueRe)
	}
	if !sandbox.ValidImage(in.Image) {
		return errBadRequest("image is not a valid image reference")
	}
	if in.Network != "none" && in.Network != "egress" {
		return errBadRequest(`network must be "none" or "egress"`)
	}
	if len(in.Command) > 64 {
		return errBadRequest("command may have at most 64 arguments")
	}
	for _, a := range in.Command {
		if len(a) > 4096 || strings.ContainsRune(a, 0) {
			return errBadRequest("command arguments must be at most 4096 bytes and contain no NUL bytes")
		}
	}
	if len(in.Env) > 64 {
		return errBadRequest("env may have at most 64 variables")
	}
	for k, v := range in.Env {
		if !sandbox.ValidEnv(k, v) {
			return errBadRequest("env variable %q is invalid: names are letters, digits and underscores; values are single-line and at most 4096 bytes", k)
		}
	}
	if in.Priority < -100 || in.Priority > 100 {
		return errBadRequest("priority must be between -100 and 100")
	}
	if in.MaxAttempts == nil {
		in.MaxAttempts = ptr(3)
	}
	if *in.MaxAttempts < 1 || *in.MaxAttempts > 20 {
		return errBadRequest("max_attempts must be between 1 and 20")
	}
	if in.TimeoutSeconds == nil {
		in.TimeoutSeconds = ptr(300)
	}
	if *in.TimeoutSeconds < 1 {
		return errBadRequest("timeout_seconds must be at least 1")
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func (s *Server) checkPolicy(r *http.Request, org store.OrgDetail, in *jobSpec) error {
	if !sandbox.ImageAllowed(s.cfg.AllowedImages, in.Image) {
		return &apiError{http.StatusForbidden, "image_not_allowed", "that image is not on this server's allowlist"}
	}
	ok, err := s.store.OrgImageAllowed(r.Context(), org.ID, in.Image)
	if err != nil {
		return err
	}
	if !ok {
		return &apiError{http.StatusForbidden, "image_not_enabled", "the image " + in.Image + " is not enabled for this organisation"}
	}
	maxTimeout := min(int(s.cfg.MaxJobTimeout.Seconds()), org.Settings.MaxTimeoutSeconds)
	if *in.TimeoutSeconds > maxTimeout {
		return errBadRequest("timeout_seconds must be at most %d for this organisation", maxTimeout)
	}
	if *in.MaxAttempts > org.Settings.MaxAttempts {
		return errBadRequest("max_attempts must be at most %d for this organisation", org.Settings.MaxAttempts)
	}
	if in.Network == "egress" {
		switch {
		case !s.cfg.Egress.Enabled():
			return &apiError{http.StatusConflict, "network_unavailable", "internet access for jobs is not set up on this server"}
		case !org.Settings.AllowNetwork:
			return &apiError{http.StatusForbidden, "network_off", "internet access is switched off for this organisation"}
		case !orgPermsOf(r).Has(perms.JobsNetwork):
			return &apiError{http.StatusForbidden, "forbidden", "you need the jobs.network permission to run jobs with internet access"}
		}
	}
	return nil
}

func (s *Server) checkQuota(r *http.Request, org store.OrgDetail, adding int) error {
	queued, err := s.store.CountQueued(r.Context(), org.ID)
	if err != nil {
		return err
	}
	if queued+adding > org.Settings.MaxPending {
		return &apiError{http.StatusTooManyRequests, "quota", fmt.Sprintf("this organisation already has %d jobs waiting (limit %d)", queued, org.Settings.MaxPending)}
	}
	return nil
}

func (in *jobSpec) newJob(orgID, createdBy string) store.NewJob {
	return store.NewJob{
		OrgID: orgID, Name: in.Name, Network: in.Network, Queue: in.Queue, Image: in.Image, Command: in.Command, Env: in.Env,
		Priority: in.Priority, MaxAttempts: *in.MaxAttempts, TimeoutSeconds: *in.TimeoutSeconds, CreatedBy: createdBy,
	}
}

func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) error {
	var in enqueueBody
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := in.checkShape(); err != nil {
		return err
	}
	if in.RunAt != nil && in.RunAt.After(time.Now().AddDate(1, 0, 0)) {
		return errBadRequest("run_at must be within one year")
	}
	if in.IdempotencyKey != nil && (len(*in.IdempotencyKey) < 1 || len(*in.IdempotencyKey) > 128) {
		return errBadRequest("idempotency_key must be 1 to 128 bytes")
	}
	org, err := s.store.GetOrg(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	if err := s.checkPolicy(r, org, &in.jobSpec); err != nil {
		return err
	}
	if err := s.checkQuota(r, org, 1); err != nil {
		return err
	}
	nj := in.newJob(org.ID, principalOf(r).actor())
	nj.RunAt, nj.IdempotencyKey = in.RunAt, in.IdempotencyKey
	job, created, err := s.store.Enqueue(r.Context(), nj)
	if err != nil {
		return err
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"job": job, "created": created})
	return nil
}

var jobStatuses = map[string]bool{
	store.StatusPending: true, store.StatusRunning: true, store.StatusSucceeded: true,
	store.StatusDead: true, store.StatusCancelled: true, store.StatusBlocked: true, store.StatusSkipped: true,
}

func jobFilter(r *http.Request) (store.JobFilter, error) {
	limit, offset, err := paging(r)
	if err != nil {
		return store.JobFilter{}, err
	}
	f := store.JobFilter{Status: r.URL.Query().Get("status"), Queue: r.URL.Query().Get("queue"), Limit: limit, Offset: offset}
	if f.Status != "" && !jobStatuses[f.Status] {
		return f, errBadRequest("unknown status %q", f.Status)
	}
	if f.Queue != "" && !queueRe.MatchString(f.Queue) {
		return f, errBadRequest("invalid queue name")
	}
	return f, nil
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) error {
	f, err := jobFilter(r)
	if err != nil {
		return err
	}
	orgID := orgIDOf(r)
	f.OrgID = &orgID
	jobs, err := s.store.ListJobs(r.Context(), f)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
	return nil
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "job")
	if err != nil {
		return err
	}
	job, err := s.store.GetJob(r.Context(), orgIDOf(r), id)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"job": job})
	return nil
}

func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "job")
	if err != nil {
		return err
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			return errBadRequest("after must be a non-negative integer")
		}
	}
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 1000 {
			return errBadRequest("limit must be between 1 and 1000")
		}
	}
	if _, err := s.store.GetJob(r.Context(), orgIDOf(r), id); err != nil {
		return err
	}
	lines, err := s.store.JobLogs(r.Context(), orgIDOf(r), id, after, limit)
	if err != nil {
		return err
	}
	next := after
	if len(lines) > 0 {
		next = lines[len(lines)-1].ID
	}
	writeJSON(w, 200, map[string]any{"logs": lines, "next_after": next})
	return nil
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "job")
	if err != nil {
		return err
	}
	job, err := s.store.CancelJob(r.Context(), orgIDOf(r), id)
	if err != nil {
		return err
	}
	s.auditOrg(r, "job.cancel", id, nil)
	status := http.StatusOK
	if job.Status == store.StatusRunning {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]any{"job": job})
	return nil
}

func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "job")
	if err != nil {
		return err
	}
	current, err := s.store.GetJob(r.Context(), orgIDOf(r), id)
	if err != nil {
		return err
	}
	org, err := s.store.GetOrg(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	spec := jobSpec{Image: current.Image, Network: current.Network, MaxAttempts: &current.MaxAttempts, TimeoutSeconds: &current.TimeoutSeconds}
	if err := s.checkPolicy(r, org, &spec); err != nil {
		return err
	}
	job, err := s.store.RetryJob(r.Context(), orgIDOf(r), id)
	if err != nil {
		return err
	}
	s.auditOrg(r, "job.retry", id, nil)
	writeJSON(w, 200, map[string]any{"job": job})
	return nil
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "job")
	if err != nil {
		return err
	}
	if err := s.store.DeleteJob(r.Context(), orgIDOf(r), id); err != nil {
		return err
	}
	s.auditOrg(r, "job.delete", id, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleOrgStats(w http.ResponseWriter, r *http.Request) error {
	counts, err := s.store.OrgStats(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"jobs_by_status": counts})
	return nil
}

func (s *Server) handleListQueues(w http.ResponseWriter, r *http.Request) error {
	queues, err := s.store.ListQueues(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"queues": queues})
	return nil
}

func (s *Server) handlePauseQueue(paused bool) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		name := r.PathValue("queue")
		if !queueRe.MatchString(name) {
			return errBadRequest("invalid queue name")
		}
		if err := s.store.SetQueuePaused(r.Context(), orgIDOf(r), name, paused); err != nil {
			return err
		}
		action := "queue.resume"
		if paused {
			action = "queue.pause"
		}
		s.auditOrg(r, action, name, nil)
		writeJSON(w, 200, map[string]any{"queue": name, "paused": paused})
		return nil
	}
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) error {
	st, err := s.store.Stats(r.Context(), 3*s.cfg.Worker.Heartbeat)
	if err != nil {
		return err
	}
	writeJSON(w, 200, st)
	return nil
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := paging(r)
	if err != nil {
		return err
	}
	users, err := s.store.ListUsers(r.Context(), limit, offset)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{"id": u.ID, "email": u.Email, "disabled": u.Disabled, "created_at": u.CreatedAt,
			"platform_permissions": perms.Platform.Names(u.PlatformPerms)})
	}
	writeJSON(w, 200, map[string]any{"users": out})
	return nil
}

func (s *Server) handleAdminOrgs(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := paging(r)
	if err != nil {
		return err
	}
	orgs, err := s.store.ListOrgs(r.Context(), limit, offset)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"orgs": orgs})
	return nil
}

func (s *Server) handleAdminJobs(w http.ResponseWriter, r *http.Request) error {
	f, err := jobFilter(r)
	if err != nil {
		return err
	}
	if v := r.URL.Query().Get("org"); v != "" {
		if !uuidRe.MatchString(v) {
			return errBadRequest("invalid org id")
		}
		f.OrgID = &v
	}
	jobs, err := s.store.ListJobs(r.Context(), f)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
	return nil
}

func (s *Server) handleAdminWorkers(w http.ResponseWriter, r *http.Request) error {
	workers, err := s.store.ListWorkers(r.Context(), 200)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"workers": workers})
	return nil
}

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := paging(r)
	if err != nil {
		return err
	}
	entries, err := s.store.ListAudit(r.Context(), nil, limit, offset)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"audit": entries})
	return nil
}

func (s *Server) handleAdminSetDisabled(disabled bool) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r, "user")
		if err != nil {
			return err
		}
		actor := principalOf(r).user
		if disabled && id == actor.ID {
			return errBadRequest("you cannot disable your own account")
		}
		if err := s.store.SetUserDisabled(r.Context(), id, disabled); err != nil {
			return err
		}
		action := "user.enable"
		if disabled {
			action = "user.disable"
		}
		s.audit(r, nil, "user:"+actor.ID, action, id, nil)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
}
