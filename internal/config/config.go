package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL string
	Address     string

	// should HSTS be enabled? disabled for local dev
	CookieSecure bool

	// force api to read X-Forwarded-For
	TrustProxy bool

	AllowSignup bool
	SessionTTL  time.Duration

	// allowed container images - i don't know if this is final solution rn
	AllowedImages []string
	MaxJobTimeout time.Duration

	Worker  Worker
	Sandbox Sandbox
}

type Worker struct {
	Concurrency  int
	PollInterval time.Duration
	ReapInterval time.Duration
	// how long a claim can be valid without a heartbeat
	Lease     time.Duration
	Heartbeat time.Duration
	// forces a SIGTERM on an overrun job after this duration
	ShutdownGrace time.Duration
	// delay before first retry - should double per run attempt
	RetryBase time.Duration
}

type Sandbox struct {
	DockerBinary string
	Runtime      string
	Memory       string
	CPUs         string
	PidsLimit    int
	TmpfsSize    string
	User         string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:   os.Getenv("NALA_DATABASE_URL"),
		Address:       str("NALA_ADDR", ":8080"),
		CookieSecure:  boolean("NALA_COOKIE_SECURE", true),
		TrustProxy:    boolean("NALA_TRUST_PROXY", false),
		AllowSignup:   boolean("NALA_ALLOW_SIGNUP", true),
		SessionTTL:    dur("NALA_SESSION_TTL", 7*24*time.Hour),
		AllowedImages: list("NALA_ALLOWED_IMAGES", "alpine:*"), // todo(@guarde): think about this list default
		MaxJobTimeout: dur("NALA_MAX_JOB_TIMEOUT", time.Hour),

		Worker: Worker{
			Concurrency:   integer("NALA_WORKER_CONCURRENCY", 4),
			PollInterval:  dur("NALA_WORKER_POLL_INTERVAL", time.Second),
			ReapInterval:  dur("NALA_WORKER_REAP_INTERVAL", 15*time.Second),
			Lease:         dur("NALA_WORKER_LEASE", 30*time.Second),
			Heartbeat:     dur("NALA_WORKER_HEARTBEAT", 10*time.Second),
			ShutdownGrace: dur("NALA_WORKER_SHUTDOWN_GRACE", 30*time.Second),
			RetryBase:     dur("NALA_WORKER_RETRY_BASE", 5*time.Second),
		},

		Sandbox: Sandbox{
			DockerBinary: str("NALA_DOCKER_BIN", "docker"),
			Runtime:      os.Getenv("NALA_SANDBOX_RUNTIME"),
			Memory:       str("NALA_SANDBOX_MEMORY", "256m"),
			CPUs:         str("NALA_SANDBOX_CPUS", "1"),
			PidsLimit:    integer("NALA_SANDBOX_PIDS", 128),
			TmpfsSize:    str("NALA_SANDBOX_TMPFS", "64m"),
			User:         str("NALA_SANDBOX_USER", "65534:65534"),
		},
	}

	if c.DatabaseURL == "" {
		return c, fmt.Errorf("NALA_DATABASE_URL is required")
	}

	if c.Worker.Concurrency < 1 {
		return c, fmt.Errorf("NALA_WORKER_CONCURRENCY must be at least 1")
	}

	if (c.Worker.Heartbeat * 2) > c.Worker.Lease {
		return c, fmt.Errorf("NALA_WORKER_LEASE must be at least twice NALA_WORKER_HEARTBEAT")
	}

	return c, nil
}

// mmm yummy casts

func str(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func boolean(k string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func integer(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func dur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

func list(k, def string) []string {
	var out []string
	for _, p := range strings.Split(str(k, def), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
