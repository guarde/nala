package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Spec struct {
	Name    string
	Image   string
	Command []string
	Env     map[string]string
}

type LogFunc func(stream, line string)

type Runner interface {
	Run(ctx context.Context, spec Spec, log LogFunc) (exitCode int, err error)
}

const (
	maxLineBytes = 8 << 10
	maxLogBytes  = 1 << 20
)

type Docker struct {
	Bin       string
	Runtime   string
	Memory    string
	CPUs      string
	PidsLimit int
	TmpfsSize string
	User      string
}

var (
	imageRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,254}(@sha256:[a-f0-9]{64})?$`)
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	nameRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

func ValidImage(ref string) bool { return imageRe.MatchString(ref) }

func ValidEnv(key, value string) bool {
	return envKeyRe.MatchString(key) && len(value) <= 4096 && !strings.ContainsAny(value, "\n\r\x00")
}

func ImageAllowed(allow []string, ref string) bool {
	for _, a := range allow {
		if p, ok := strings.CutSuffix(a, "*"); ok {
			if p != "" && strings.HasPrefix(ref, p) {
				return true
			}
		} else if a == ref {
			return true
		}
	}
	return false
}

func (d Docker) Args(spec Spec, envFile string) ([]string, error) {
	if !nameRe.MatchString(spec.Name) {
		return nil, fmt.Errorf("sandbox: invalid container name %q", spec.Name)
	}
	if !ValidImage(spec.Image) {
		return nil, fmt.Errorf("sandbox: invalid image reference %q", spec.Image)
	}
	args := []string{
		"run", "--rm",
		"--name", spec.Name,
		"--label", "nala.managed=true",
		"--network", "none",
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=" + d.TmpfsSize,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", d.User,
		"--pids-limit", fmt.Sprint(d.PidsLimit),
		"--memory", d.Memory,
		"--memory-swap", d.Memory,
		"--cpus", d.CPUs,
		"--ipc", "none",
		"--pull", "missing",
	}
	if d.Runtime != "" {
		args = append(args, "--runtime", d.Runtime)
	}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, spec.Image)
	return append(args, spec.Command...), nil
}

func (d Docker) Run(ctx context.Context, spec Spec, log LogFunc) (int, error) {
	envFile, cleanup, err := writeEnvFile(spec.Env)
	if err != nil {
		return -1, err
	}
	defer cleanup()
	args, err := d.Args(spec, envFile)
	if err != nil {
		return -1, err
	}

	cmd := exec.CommandContext(ctx, d.Bin, args...)
	cmd.Cancel = func() error {
		d.kill(spec.Name)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 10 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("sandbox: start docker: %w", err)
	}

	budget := &logBudget{left: maxLogBytes, log: log}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(stdout, "stdout", budget) }()
	go func() { defer wg.Done(); pump(stderr, "stderr", budget) }()
	wg.Wait()
	err = cmd.Wait()

	if ctx.Err() != nil {
		d.kill(spec.Name)
		return -1, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("sandbox: %w", err)
	}
	return 0, nil
}

func (d Docker) kill(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, d.Bin, "rm", "--force", name).Run()
}

func writeEnvFile(env map[string]string) (path string, cleanup func(), err error) {
	if len(env) == 0 {
		return "", func() {}, nil
	}
	keys := make([]string, 0, len(env))
	for k, v := range env {
		if !ValidEnv(k, v) {
			return "", nil, fmt.Errorf("sandbox: invalid environment variable %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	f, err := os.CreateTemp("", "nala-env-*")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.Remove(f.Name()) }
	for _, k := range keys {
		if _, err := fmt.Fprintf(f, "%s=%s\n", k, env[k]); err != nil {
			f.Close()
			cleanup()
			return "", nil, err
		}
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

type logBudget struct {
	mu   sync.Mutex
	left int
	done bool
	log  LogFunc
}

func (b *logBudget) emit(stream, line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return
	}
	if len(line) > b.left {
		b.done = true
		b.log("system", "log limit reached; further output dropped")
		return
	}
	b.left -= len(line) + 1
	b.log(stream, line)
}

func pump(r io.Reader, stream string, b *logBudget) {
	br := bufio.NewReaderSize(r, maxLineBytes)
	for {
		line, isPrefix, err := br.ReadLine()
		if err != nil {
			return
		}
		text := strings.ToValidUTF8(strings.ReplaceAll(string(line), "\x00", ""), "�")
		if isPrefix {
			text += " [line truncated]"
			for isPrefix && err == nil {
				_, isPrefix, err = br.ReadLine()
			}
		}
		b.emit(stream, text)
		if err != nil {
			return
		}
	}
}
