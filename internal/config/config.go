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
	// how long a claim can be valid without a heartbeat
	Lease        time.Duration
	Heartbeat    time.Duration
	ReapInterval time.Duration
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

		Worker:  Worker{},
		Sandbox: Sandbox{},
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
