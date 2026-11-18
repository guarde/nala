package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"sync"
	"time"

	"guarde.sh/nala/internal/config"
	"guarde.sh/nala/internal/sandbox"
	"guarde.sh/nala/internal/store"
)

var (
	errCancelRequested = errors.New("cancel requested")
	errLeaseLost       = errors.New("lease lost")
	errShutdown        = errors.New("worker shutting down")
)

type Worker struct {
	ID     string
	store  *store.Store
	runner sandbox.Runner
	cfg    config.Worker
	allow  []string
	log    *slog.Logger

	mu      sync.Mutex
	running map[string]context.CancelCauseFunc
}

func New(st *store.Store, runner sandbox.Runner, cfg config.Worker, allowedImages []string, log *slog.Logger) *Worker {
	host, _ := os.Hostname()
	suffix := make([]byte, 4)
	rand.Read(suffix)
	id := fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(suffix))
	return &Worker{
		ID: id, store: st, runner: runner, cfg: cfg, allow: allowedImages,
		log:     log.With("worker", id),
		running: map[string]context.CancelCauseFunc{},
	}
}

func (w *Worker) Run(ctx context.Context) error {
	host, _ := os.Hostname()
	if err := w.store.RegisterWorker(ctx, w.ID, host, w.cfg.Concurrency); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	w.log.Info("worker started", "concurrency", w.cfg.Concurrency)

	jobsCtx, cancelJobs := context.WithCancelCause(context.Background())
	defer cancelJobs(nil)
	bgCtx, stopBackground := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { defer bg.Done(); w.heartbeatLoop(bgCtx) }()
	go func() { defer bg.Done(); w.reapLoop(bgCtx) }()

	var slots sync.WaitGroup
	for i := 0; i < w.cfg.Concurrency; i++ {
		slots.Add(1)
		go func() { defer slots.Done(); w.slot(ctx, jobsCtx) }()
	}

	<-ctx.Done()
	w.log.Info("shutdown requested; waiting for running jobs", "grace", w.cfg.ShutdownGrace, "running", w.runningCount())
	done := make(chan struct{})
	go func() { slots.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.cfg.ShutdownGrace):
		w.log.Warn("grace period over; stopping remaining jobs", "running", w.runningCount())
		cancelJobs(errShutdown)
		<-done
	}

	stopBackground()
	bg.Wait()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.store.StopWorker(stopCtx, w.ID); err != nil {
		w.log.Error("mark worker stopped", "err", err)
	}
	w.log.Info("worker stopped")
	return nil
}

func (w *Worker) runningCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.running)
}

func (w *Worker) slot(ctx, jobsCtx context.Context) {
	for ctx.Err() == nil {
		job, err := w.store.Claim(ctx, w.ID, w.cfg.Lease)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.Error("claim", "err", err)
			sleep(ctx, 5*time.Second)
			continue
		}
		if job == nil {
			sleep(ctx, w.cfg.PollInterval+time.Duration(mrand.Int64N(int64(w.cfg.PollInterval/2)+1)))
			continue
		}
		w.execute(jobsCtx, job)
	}
}

func (w *Worker) execute(jobsCtx context.Context, job *store.Job) {
	log := w.log.With("job", job.ID, "attempt", job.Attempts)
	timeout := time.Duration(job.TimeoutSeconds) * time.Second

	ctx, cancel := context.WithCancelCause(jobsCtx)
	defer cancel(nil)
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()

	w.mu.Lock()
	w.running[job.ID] = cancel
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.running, job.ID)
		w.mu.Unlock()
	}()

	logs := newLogSink(w.store, job.ID, job.Attempts, log)
	logs.add("system", fmt.Sprintf("attempt %d/%d started on worker %s", job.Attempts, job.MaxAttempts, w.ID))
	log.Info("job started", "image", job.Image)

	exitCode, runErr := w.safeRun(ctx, job, logs.add)

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer dbCancel()

	var err error
	cause := context.Cause(ctx)
	switch {
	case runErr == nil && exitCode == 0:
		logs.add("system", "job succeeded")
		logs.close()
		err = w.store.Succeed(dbCtx, job, w.ID)
		log.Info("job succeeded")

	case errors.Is(cause, errLeaseLost):
		logs.add("system", "attempt abandoned: this worker lost its lease")
		logs.close()
		log.Warn("job abandoned after losing lease")

	case errors.Is(cause, errCancelRequested):
		logs.add("system", "job cancelled by request")
		logs.close()
		err = w.store.MarkCancelled(dbCtx, job, w.ID)
		log.Info("job cancelled")

	case errors.Is(cause, errShutdown):
		logs.add("system", "worker shutting down; job released back to the queue")
		logs.close()
		err = w.store.Release(dbCtx, job, w.ID)
		log.Info("job released")

	default:
		var reason string
		var code *int
		switch {
		case errors.Is(cause, context.DeadlineExceeded):
			reason = fmt.Sprintf("timed out after %s", timeout)
		case runErr != nil:
			reason = runErr.Error()
		default:
			reason = fmt.Sprintf("exited with code %d", exitCode)
			code = &exitCode
		}
		backoff := Backoff(w.cfg.RetryBase, job.Attempts)
		var status string
		status, err = w.store.Fail(dbCtx, job, w.ID, reason, code, backoff)
		switch status {
		case store.StatusDead:
			logs.add("system", "attempt failed: "+reason+"; no attempts left, job is dead")
		case store.StatusPending:
			logs.add("system", fmt.Sprintf("attempt failed: %s; retrying in %s", reason, backoff.Round(time.Second)))
		}
		logs.close()
		log.Warn("job failed", "reason", reason, "status", status)
	}
	if errors.Is(err, store.ErrNotFound) {
		log.Warn("result discarded: job is no longer held by this worker")
	} else if err != nil {
		log.Error("could not record job result", "err", err)
	}
}

func (w *Worker) safeRun(ctx context.Context, job *store.Job, logf sandbox.LogFunc) (code int, err error) {
	defer func() {
		if r := recover(); r != nil {
			code, err = -1, fmt.Errorf("runner panicked: %v", r)
		}
	}()
	if !sandbox.ImageAllowed(w.allow, job.Image) {
		return -1, fmt.Errorf("image %q is not on the allowlist", job.Image)
	}
	return w.runner.Run(ctx, sandbox.Spec{
		Name:    fmt.Sprintf("nala-%s-%d", job.ID, job.Attempts),
		Image:   job.Image,
		Command: job.Command,
		Env:     job.Env,
	}, logf)
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(w.cfg.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.mu.Lock()
		mine := make(map[string]context.CancelCauseFunc, len(w.running))
		for id, c := range w.running {
			mine[id] = c
		}
		w.mu.Unlock()

		hbCtx, cancel := context.WithTimeout(ctx, w.cfg.Heartbeat)
		held, err := w.store.Heartbeat(hbCtx, w.ID, w.cfg.Lease)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("heartbeat", "err", err)
			}
			continue
		}
		for id, cancelJob := range mine {
			cancelRequested, stillHeld := held[id]
			switch {
			case !stillHeld:
				cancelJob(errLeaseLost)
			case cancelRequested:
				cancelJob(errCancelRequested)
			}
		}
	}
}

func (w *Worker) reapLoop(ctx context.Context) {
	t := time.NewTicker(w.cfg.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := w.store.Reap(ctx)
			if err != nil && ctx.Err() == nil {
				w.log.Error("reap", "err", err)
			} else if n > 0 {
				w.log.Warn("recovered jobs from dead workers", "count", n)
			}
		}
	}
}

func Backoff(base time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Hour
	if attempt <= 16 {
		d = min(base<<(attempt-1), time.Hour)
	}
	return d + time.Duration(mrand.Int64N(int64(d/5)+1))
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

type logSink struct {
	store   *store.Store
	jobID   string
	attempt int
	log     *slog.Logger

	mu   sync.Mutex
	buf  []store.LogLine
	stop chan struct{}
	done chan struct{}
}

func newLogSink(st *store.Store, jobID string, attempt int, log *slog.Logger) *logSink {
	s := &logSink{store: st, jobID: jobID, attempt: attempt, log: log, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.flush()
			case <-s.stop:
				s.flush()
				return
			}
		}
	}()
	return s
}

func (s *logSink) add(stream, line string) {
	s.mu.Lock()
	s.buf = append(s.buf, store.LogLine{Stream: stream, Line: line, TS: time.Now()})
	s.mu.Unlock()
}

func (s *logSink) flush() {
	s.mu.Lock()
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.AppendLogs(ctx, s.jobID, s.attempt, batch); err != nil {
		s.log.Error("write job logs", "err", err, "lines", len(batch))
	}
}

func (s *logSink) close() {
	close(s.stop)
	<-s.done
}
