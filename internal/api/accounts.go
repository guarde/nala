package api

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"guarde.sh/nala/internal/auth"
	"guarde.sh/nala/internal/perms"
	"guarde.sh/nala/internal/store"
	"guarde.sh/nala/pkg/bitperms"
)

func normalizeEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 {
		return "", errBadRequest("invalid email address")
	}
	return e, nil
}

func validName(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || utf8.RuneCountInString(v) > 100 || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errBadRequest("%s must be 1 to 100 printable characters", field)
	}
	return v, nil
}

func permissionView(set *bitperms.Set, p bitperms.Perm) map[string]any {
	v := map[string]any{"permissions": set.Names(p)}
	if set == perms.Org {
		v["role"] = perms.RoleName(p)
	}
	return v
}

func parseGrant(role string, names []string) (bitperms.Perm, error) {
	switch {
	case role != "" && names != nil:
		return 0, errBadRequest("give either role or permission - not both")
	case role != "":
		p, ok := perms.Roles[role]
		if !ok {
			return 0, errBadRequest("unknown role %q", role)
		}
		return p, nil
	case names != nil:
		p, err := perms.Org.Parse(names)
		if err != nil {
			return 0, errBadRequest("%v", err)
		}
		return p, nil
	}
	return 0, errBadRequest("role or permission is rqeuired")
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string) error {
	token, hash, err := auth.NewToken(auth.SessionPrefix)
	if err != nil {
		return err
	}

	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}

	if err := s.store.CreateSession(r.Context(), hash, userID, s.cfg.SessionTTL, s.clientIP(r), strings.ToValidUTF8(ua, "")); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) error {
	if !s.cfg.AllowSignup {
		return &apiError{http.StatusForbidden, "signup_disabled", "sign-up is disabled"}
	}

	if !s.SignupByIP.Allow(s.clientIP(r)) {
		return errRateLimited
	}

	var in credentials
	if err := readJSON(r, &in); err != nil {
		return err
	}

	email, err := normalizeEmail(in.Email)
	if err != nil {
		return err
	}

	if err := auth.CheckPasswordPolicy(in.Password); err != nil {
		return errBadRequest("%v", err)
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return err
	}

	u, err := s.store.CreateUser(r.Context(), email, hash, bitperms.None)
	if errors.Is(err, store.ErrConflict) {
		return &apiError{http.StatusConflict, "email_taken", "an account with that email already existss"}
	}

	if err != nil {
		return err
	}

	s.audit(r, nil, "user:"+u.ID, "user.register", u.ID, nil)
	if err := s.startSession(w, r, u.ID); err != nil {
		return err
	}

	writeJSON(w, http.StatusCreated, map[string]any{"user": u})
	return nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) error {
	var in credentials
	if err := readJSON(r, &in); err != nil {
		return err
	}

	email := strings.ToLower(strings.TrimSpace(in.Email))

	if !s.LoginByIP.Allow(s.clientIP(r)) || !s.LoginByEmail.Allow(email) {
		return errRateLimited
	}

	bad := &apiError{http.StatusUnauthorized, "invalid_credentials", "invalid email or password"}

	u, err := s.store.UserByEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		auth.BurnPasswordCheck(in.Password)
		s.audit(r, nil, "anonymous", "login.failed", "", nil)
		return bad
	}

	if err != nil {
		return err
	}

	if !auth.VerifyPassword(u.PasswordHash, in.Password) || u.Disabled {
		s.audit(r, nil, "user:"+u.ID, "login.failed", u.ID, nil)
		return bad
	}

	if err := s.startSession(w, r, u.ID); err != nil {
		return err
	}

	s.audit(r, nil, "user:"+u.ID, "login.ok", u.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	if err := s.store.DeleteSession(r.Context(), principalOf(r).sessionHash); err != nil {
		return err
	}
	s.clearSession(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}

	if err := readJSON(r, &in); err != nil {
		return err
	}

	u := principalOf(r).user
	if !s.LoginByEmail.Allow(u.Email) {
		return errRateLimited
	}

	if !auth.VerifyPassword(u.PasswordHash, in.Current) {
		return &apiError{http.StatusForbidden, "invalid_credentials", "current password is incorrect"}
	}

	if err := auth.CheckPasswordPolicy(in.New); err != nil {
		return errBadRequest("%v", err)
	}

	hash, err := auth.HashPassword(in.New)
	if err != nil {
		return err
	}

	if err := s.store.SetPassword(r.Context(), u.ID, hash); err != nil {
		return err
	}

	if err := s.store.DeleteUserSessions(r.Context(), u.ID); err != nil {
		return err
	}

	s.audit(r, nil, "user:"+u.ID, "user.password_changed", u.ID, nil)
	if err := s.startSession(w, r, u.ID); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	u := principalOf(r).user

	orgs, err := s.store.OrgsForUser(r.Context(), u.ID)
	if err != nil {
		return err
	}

	out := make([]map[string]any, 0, len(orgs))
	for _, o := range orgs {
		v := permissionView(perms.Org, o.Perms)
		v["id"], v["name"] = o.ID, o.Name
		out = append(out, v)
	}

	writeJSON(w, 200, map[string]any{
		"user":                 u,
		"platform_permissions": perms.Platform.Names(u.PlatformPerms),
		"orgs":                 out,
	})
	return nil
}

func (s *Server) handlePermissionCatalogue(w http.ResponseWriter, r *http.Request) error {
	roles := map[string][]string{}
	for name, p := range perms.Roles {
		roles[name] = perms.Org.Names(p)
	}

	writeJSON(w, 200, map[string]any{
		"permissions":         perms.Org.Names(perms.Org.All()),
		"roles":               roles,
		"api_key_permissions": perms.Org.Names(perms.KeyAllowed),
	})
	return nil
}

func (s *Server) handleListOrgs(w http.ResponseWriter, r *http.Request) error {
	orgs, err := s.store.OrgsForUser(r.Context(), principalOf(r).user.ID)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(orgs))
	for _, o := range orgs {
		v := permissionView(perms.Org, o.Perms)
		v["id"], v["name"], v["created_at"] = o.ID, o.Name, o.CreatedAt
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"orgs": out})
	return nil
}

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name, err := validName("name", in.Name)
	if err != nil {
		return err
	}
	u := principalOf(r).user
	o, err := s.store.CreateOrg(r.Context(), name, u.ID, perms.RoleOwner)
	if err != nil {
		return err
	}
	s.audit(r, &o.ID, "user:"+u.ID, "org.create", o.ID, map[string]string{"name": name})
	writeJSON(w, http.StatusCreated, map[string]any{"org": o})
	return nil
}

func (s *Server) handleRenameOrg(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name, err := validName("name", in.Name)
	if err != nil {
		return err
	}
	if err := s.store.RenameOrg(r.Context(), orgIDOf(r), name); err != nil {
		return err
	}
	s.auditOrg(r, "org.rename", orgIDOf(r), map[string]string{"name": name})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func memberView(m store.Member) map[string]any {
	v := permissionView(perms.Org, m.Perms)
	v["user_id"], v["email"], v["joined_at"] = m.UserID, m.Email, m.CreatedAt
	return v
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) error {
	members, err := s.store.ListMembers(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		out = append(out, memberView(m))
	}
	writeJSON(w, 200, map[string]any{"members": out})
	return nil
}

type grantBody struct {
	Email       string   `json:"email,omitempty"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"`
}

var errEscalation = &apiError{http.StatusForbidden, "escalation", "you cannot grant permissions you do not hold yourself"}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) error {
	var in grantBody
	if err := readJSON(r, &in); err != nil {
		return err
	}
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return err
	}
	grant, err := parseGrant(in.Role, in.Permissions)
	if err != nil {
		return err
	}
	if !bitperms.CanGrant(orgPermsOf(r), grant) {
		return errEscalation
	}
	u, err := s.store.UserByEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		return &apiError{http.StatusNotFound, "user_not_found", "no user with that email; they need to sign up first"}
	}
	if err != nil {
		return err
	}
	if err := s.store.AddMember(r.Context(), orgIDOf(r), u.ID, grant); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return &apiError{http.StatusConflict, "already_member", "that user is already a member"}
		}
		return err
	}
	s.auditOrg(r, "member.add", u.ID, permissionView(perms.Org, grant))
	writeJSON(w, http.StatusCreated, map[string]any{"member": memberView(store.Member{UserID: u.ID, Email: u.Email, Perms: grant, CreatedAt: time.Now()})})
	return nil
}

func (s *Server) targetMember(r *http.Request) (userID string, current bitperms.Perm, err error) {
	if userID, err = pathID(r, "user"); err != nil {
		return
	}
	if current, err = s.store.MembershipPerms(r.Context(), orgIDOf(r), userID); err != nil {
		return
	}
	if !orgPermsOf(r).Has(current) {
		err = &apiError{http.StatusForbidden, "forbidden", "you cannot manage a member who holds permissions you do not"}
	}
	return
}

func (s *Server) handleUpdateMember(w http.ResponseWriter, r *http.Request) error {
	var in grantBody
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.Email != "" {
		return errBadRequest("email cannot be changed here")
	}
	grant, err := parseGrant(in.Role, in.Permissions)
	if err != nil {
		return err
	}
	userID, current, err := s.targetMember(r)
	if err != nil {
		return err
	}
	if !bitperms.CanGrant(orgPermsOf(r), grant) {
		return errEscalation
	}
	if err := s.store.ChangeMember(r.Context(), orgIDOf(r), userID, &grant, perms.RoleOwner); err != nil {
		return err
	}
	s.auditOrg(r, "member.update", userID, map[string]any{"from": perms.Org.Names(current), "to": perms.Org.Names(grant)})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) error {
	userID, _, err := s.targetMember(r)
	if err != nil {
		return err
	}
	if err := s.store.ChangeMember(r.Context(), orgIDOf(r), userID, nil, perms.RoleOwner); err != nil {
		return err
	}
	s.auditOrg(r, "member.remove", userID, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func keyView(k store.APIKey) map[string]any {
	return map[string]any{
		"id": k.ID, "name": k.Name, "hint": "…" + k.Hint, "permissions": perms.Org.Names(k.Perms),
		"created_at": k.CreatedAt, "expires_at": k.ExpiresAt, "last_used_at": k.LastUsedAt, "revoked_at": k.RevokedAt,
	}
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) error {
	keys, err := s.store.ListAPIKeys(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyView(k))
	}
	writeJSON(w, 200, map[string]any{"api_keys": out})
	return nil
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) error {
	p := principalOf(r)
	if p.user == nil {
		return errForbidden
	}
	var in struct {
		Name          string   `json:"name"`
		Permissions   []string `json:"permissions"`
		ExpiresInDays *int     `json:"expires_in_days"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name, err := validName("name", in.Name)
	if err != nil {
		return err
	}
	grant, err := parseGrant("", in.Permissions)
	if err != nil {
		return err
	}
	if grant == bitperms.None {
		return errBadRequest("an API key needs at least one permission")
	}
	if !perms.KeyAllowed.Has(grant) {
		return errBadRequest("API keys may only hold: %s", strings.Join(perms.Org.Names(perms.KeyAllowed), ", "))
	}
	if !bitperms.CanGrant(orgPermsOf(r), grant) {
		return errEscalation
	}
	var expires *time.Time
	if in.ExpiresInDays != nil {
		if *in.ExpiresInDays < 1 || *in.ExpiresInDays > 3650 {
			return errBadRequest("expires_in_days must be between 1 and 3650")
		}
		t := time.Now().AddDate(0, 0, *in.ExpiresInDays)
		expires = &t
	}
	token, hash, err := auth.NewToken(auth.APIKeyPrefix)
	if err != nil {
		return err
	}
	k, err := s.store.CreateAPIKey(r.Context(), orgIDOf(r), name, hash, token[len(token)-4:], grant, p.user.ID, expires)
	if err != nil {
		return err
	}
	s.auditOrg(r, "apikey.create", k.ID, map[string]any{"name": name, "permissions": perms.Org.Names(grant)})
	v := keyView(k)
	v["key"] = token
	writeJSON(w, http.StatusCreated, map[string]any{"api_key": v})
	return nil
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) error {
	keyID, err := pathID(r, "key")
	if err != nil {
		return err
	}
	if err := s.store.RevokeAPIKey(r.Context(), orgIDOf(r), keyID); err != nil {
		return err
	}
	s.auditOrg(r, "apikey.revoke", keyID, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleOrgAudit(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := paging(r)
	if err != nil {
		return err
	}
	orgID := orgIDOf(r)
	entries, err := s.store.ListAudit(r.Context(), &orgID, limit, offset)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"audit": entries})
	return nil
}
