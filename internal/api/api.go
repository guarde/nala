package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"guarde.sh/nala/internal/config"
	"guarde.sh/nala/internal/store"
	"guarde.sh/nala/pkg/bitperms"
)

// body bites?? kinky.
const maxBodyBytes = 512 << 10

type Server struct {
	store *store.Store
	cfg   config.Config
	log   *slog.Logger

	LoginByIP    *Limiter
	LoginByEmail *Limiter
	SignupByIP   *Limiter
}

func New(st *store.Store, cfg config.Config, log *slog.Logger) *Server {
	return &Server{
		store: st, cfg: cfg, log: log,
		LoginByIP:    NewLimiter(20, time.Minute),
		LoginByEmail: NewLimiter(10, 10*time.Minute),
		SignupByIP:   NewLimiter(10, time.Hour),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn handler) { mux.Handle(pattern, s.wrap(fn)) }

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	h("GET /ready", s.handleReady)

	return s.recoverer(s.requestLog(s.securityHeaders(s.crossOriginGuard(mux))))
}

type handler func(w http.ResponseWriter, r *http.Request) error

type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Message }

func errBadRequest(format string, a ...any) error {
	return &apiError{http.StatusBadRequest, "bad_request", fmt.Sprintf(format, a...)}
}

var (
	errUnauthorized = &apiError{http.StatusUnauthorized, "unauthorized", "authentication required"}
	errForbidden    = &apiError{http.StatusForbidden, "forbidden", "you do not have permission to do that"}
	errNotFound     = &apiError{http.StatusNotFound, "not_found", "not found"}
	errRateLimited  = &apiError{http.StatusTooManyRequests, "rate_limited", "too many attempts; try again later"}
)

func (s *Server) wrap(fn handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		err := fn(w, r)
		if err == nil {
			return
		}
		var ae *apiError
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &ae):
		case errors.As(err, &tooBig):
			ae = &apiError{http.StatusRequestEntityTooLarge, "too_large", "request body too large"}
		case errors.Is(err, store.ErrNotFound):
			ae = errNotFound
		case errors.Is(err, store.ErrConflict):
			ae = &apiError{http.StatusConflict, "conflict", "the resource is not in a state that allows this"}
		case errors.Is(err, store.ErrLastOwner):
			ae = &apiError{http.StatusConflict, "last_owner", err.Error()}
		default:
			s.log.Error("request failed", "err", err, "method", r.Method, "path", r.URL.Path, "request_id", w.Header().Get("X-Request-Id"))
			ae = &apiError{http.StatusInternalServerError, "internal", "internal error"}
		}
		writeJSON(w, ae.Status, map[string]any{"error": ae})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return &apiError{http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"}
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return errBadRequest("invalid JSON body: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errBadRequest("request body must contain a single JSON object")
	}
	return nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func pathID(r *http.Request, name string) (string, error) {
	v := strings.ToLower(r.PathValue(name))
	if !uuidRe.MatchString(v) {
		return "", errNotFound
	}
	return v, nil
}

func paging(r *http.Request) (limit, offset int, err error) {
	limit, offset = 50, 0
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 200 {
			return 0, 0, errBadRequest("limit must be between 1 and 200")
		}
	}
	if v := q.Get("offset"); v != "" {
		if offset, err = strconv.Atoi(v); err != nil || offset < 0 || offset > 100_000 {
			return 0, 0, errBadRequest("offset must be between 0 and 100000")
		}
	}
	return limit, offset, nil
}

type principal struct {
	user        *store.User
	key         *store.APIKey
	sessionHash []byte
}

func (p *principal) actor() string {
	if p.key != nil {
		return "key:" + p.key.ID
	}
	return "user:" + p.user.ID
}

type ctxKey int

const (
	ctxPrincipal ctxKey = iota
	ctxOrgID
	ctxOrgPerms
)

func principalOf(r *http.Request) *principal { return r.Context().Value(ctxPrincipal).(*principal) }
func orgIDOf(r *http.Request) string         { return r.Context().Value(ctxOrgID).(string) }
func orgPermsOf(r *http.Request) bitperms.Perm {
	return r.Context().Value(ctxOrgPerms).(bitperms.Perm)
}

func (s *Server) cookieName() string {
	if s.cfg.CookieSecure {
		return "__Host-nala_session"
	}
	return "nala_session"
}

func (s *Server) authenticate(r *http.Request) (*principal, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		token, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || !strings.HasPrefix(token, auth.APIKeyPrefix) || len(token) > 200 {
			return nil, errUnauthorized
		}
		k, err := s.store.APIKeyByHash(r.Context(), auth.HashToken(token))
		if errors.Is(err, store.ErrNotFound) {
			return nil, errUnauthorized
		}
		if err != nil {
			return nil, err
		}
		return &principal{key: &k}, nil
	}
	c, err := r.Cookie(s.cookieName())
	if err != nil || !strings.HasPrefix(c.Value, auth.SessionPrefix) || len(c.Value) > 200 {
		return nil, errUnauthorized
	}
	hash := auth.HashToken(c.Value)
	u, err := s.store.SessionUser(r.Context(), hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errUnauthorized
	}
	if err != nil {
		return nil, err
	}
	return &principal{user: &u, sessionHash: hash}, nil
}

func (s *Server) user(next handler) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.authenticate(r)
		if err != nil {
			return err
		}
		if p.user == nil {
			return errForbidden
		}
		return next(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
	}
}

func (s *Server) org(need bitperms.Perm, next handler) handler {
	if need == bitperms.None {
		panic("api: org route registered without a required permission")
	}
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.authenticate(r)
		if err != nil {
			return err
		}
		orgID, err := pathID(r, "org")
		if err != nil {
			return err
		}
		var have bitperms.Perm
		if p.key != nil {
			if p.key.OrgID != orgID {
				return errNotFound
			}
			have = p.key.Perms & perms.KeyAllowed
		} else {
			have, err = s.store.MembershipPerms(r.Context(), orgID, p.user.ID)
			if errors.Is(err, store.ErrNotFound) {
				return errNotFound
			}
			if err != nil {
				return err
			}
		}
		if !have.Has(need) {
			return errForbidden
		}
		ctx := context.WithValue(r.Context(), ctxPrincipal, p)
		ctx = context.WithValue(ctx, ctxOrgID, orgID)
		ctx = context.WithValue(ctx, ctxOrgPerms, have)
		return next(w, r.WithContext(ctx))
	}
}

func (s *Server) admin(need bitperms.Perm, next handler) handler {
	if need == bitperms.None {
		panic("api: admin route registered without a required permission")
	}
	return s.user(func(w http.ResponseWriter, r *http.Request) error {
		if !principalOf(r).user.PlatformPerms.Has(need) {
			return errNotFound
		}
		return next(w, r)
	})
}

func (s *Server) audit(r *http.Request, orgID *string, actor, action, target string, detail any) {
	if err := s.store.Audit(r.Context(), orgID, actor, action, target, detail, s.clientIP(r)); err != nil {
		s.log.Error("write audit log", "err", err, "action", action)
	}
}

func (s *Server) auditOrg(r *http.Request, action, target string, detail any) {
	orgID := orgIDOf(r)
	s.audit(r, &orgID, principalOf(r).actor(), action, target, detail)
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) crossOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		if !safe && r.Header.Get("Authorization") == "" && !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": &apiError{Code: "cross_origin", Message: "cross-origin request blocked"}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := make([]byte, 8)
		rand.Read(id)
		rid := hex.EncodeToString(id)
		w.Header().Set("X-Request-Id", rid)
		sw := &statusWriter{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(sw, r)
		s.log.Info("request", "request_id", rid, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "ms", time.Since(start).Milliseconds(), "ip", s.clientIP(r))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic in handler", "panic", rec, "path", r.URL.Path)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": &apiError{Code: "internal", Message: "internal error"}})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.DB.PingContext(ctx); err != nil {
		return &apiError{http.StatusServiceUnavailable, "unavailable", "database unavailable"}
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
	return nil
}
