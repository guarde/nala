package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"guarde.sh/nala/internal/auth"
	"guarde.sh/nala/internal/egress"
	"guarde.sh/nala/internal/perms"
	"guarde.sh/nala/internal/sandbox"
	"guarde.sh/nala/internal/store"
	"guarde.sh/nala/pkg/bitperms"
)

func (s *Server) handleGetOrg(w http.ResponseWriter, r *http.Request) error {
	org, err := s.store.GetOrg(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	v := permissionView(perms.Org, orgPermsOf(r))
	v["org"] = org
	v["server"] = map[string]any{
		"internet_available":  s.cfg.Egress.Enabled(),
		"max_timeout_seconds": int(s.cfg.MaxJobTimeout.Seconds()),
	}
	writeJSON(w, 200, v)
	return nil
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) error {
	org, err := s.store.GetOrg(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	var in struct {
		AllowNetwork      *bool `json:"allow_network"`
		MaxTimeoutSeconds *int  `json:"max_timeout_seconds"`
		MaxAttempts       *int  `json:"max_attempts"`
		MaxRunning        *int  `json:"max_running"`
		MaxPending        *int  `json:"max_pending"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	st := org.Settings
	set := func(dst *int, src *int, name string, lo, hi int) error {
		if src == nil {
			return nil
		}
		if *src < lo || *src > hi {
			return errBadRequest("%s must be between %d and %d", name, lo, hi)
		}
		*dst = *src
		return nil
	}
	serverMax := int(s.cfg.MaxJobTimeout.Seconds())
	if err := errors.Join(
		set(&st.MaxTimeoutSeconds, in.MaxTimeoutSeconds, "max_timeout_seconds", 1, serverMax),
		set(&st.MaxAttempts, in.MaxAttempts, "max_attempts", 1, 20),
		set(&st.MaxRunning, in.MaxRunning, "max_running", 0, 1000),
		set(&st.MaxPending, in.MaxPending, "max_pending", 0, 1_000_000),
	); err != nil {
		return errBadRequest("%v", err)
	}
	if in.AllowNetwork != nil {
		st.AllowNetwork = *in.AllowNetwork
	}
	if err := s.store.UpdateOrgSettings(r.Context(), org.ID, st); err != nil {
		return err
	}
	s.auditOrg(r, "org.settings", org.ID, st)
	writeJSON(w, 200, map[string]any{"settings": st})
	return nil
}

func (s *Server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) error {
	if principalOf(r).user == nil {
		return errForbidden
	}
	var in struct {
		ConfirmName string `json:"confirm_name"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	org, err := s.store.GetOrg(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	if in.ConfirmName != org.Name {
		return errBadRequest("confirm_name must match the organisation's name exactly")
	}
	if err := s.store.DeleteOrg(r.Context(), org.ID); err != nil {
		return err
	}
	s.audit(r, nil, principalOf(r).actor(), "org.delete", org.ID, map[string]string{"name": org.Name})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleLeaveOrg(w http.ResponseWriter, r *http.Request) error {
	p := principalOf(r)
	if p.user == nil {
		return errForbidden
	}
	if err := s.store.ChangeMember(r.Context(), orgIDOf(r), p.user.ID, nil, perms.RoleOwner); err != nil {
		return err
	}
	s.auditOrg(r, "member.leave", p.user.ID, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleOrgCatalogue(c interface {
	ForOrg(ctx context.Context, s *store.Store, orgID string) ([]store.CatalogueItem, error)
}, key string) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		items, err := c.ForOrg(r.Context(), s.store, orgIDOf(r))
		if err != nil {
			return err
		}
		if key == "images" {
			kept := items[:0]
			for _, it := range items {
				if sandbox.ImageAllowed(s.cfg.AllowedImages, it.Value) {
					kept = append(kept, it)
				}
			}
			items = kept
		}
		writeJSON(w, 200, map[string]any{key: items})
		return nil
	}
}

func (s *Server) handleSetOrgCatalogue(c interface {
	SetForOrg(ctx context.Context, s *store.Store, orgID, id string, enabled bool) error
}, action, param string) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r, param)
		if err != nil {
			return err
		}
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if in.Enabled == nil {
			return errBadRequest("enabled is required")
		}
		if err := c.SetForOrg(r.Context(), s.store, orgIDOf(r), id, *in.Enabled); err != nil {
			return err
		}
		s.auditOrg(r, action, id, map[string]bool{"enabled": *in.Enabled})
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
}

func inviteView(i store.Invite) map[string]any {
	v := permissionView(perms.Org, i.Perms)
	v["id"], v["email"], v["invited_by"], v["created_at"], v["expires_at"] = i.ID, i.Email, i.InvitedBy, i.CreatedAt, i.ExpiresAt
	return v
}

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) error {
	invites, err := s.store.ListInvites(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(invites))
	for _, i := range invites {
		out = append(out, inviteView(i))
	}
	writeJSON(w, 200, map[string]any{"invites": out})
	return nil
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) error {
	p := principalOf(r)
	if p.user == nil {
		return errForbidden
	}
	var in struct {
		Email         string   `json:"email"`
		Role          string   `json:"role"`
		Permissions   []string `json:"permissions"`
		ExpiresInDays *int     `json:"expires_in_days"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	email := ""
	if strings.TrimSpace(in.Email) != "" {
		var err error
		if email, err = normalizeEmail(in.Email); err != nil {
			return err
		}
	}
	grant, err := parseGrant(in.Role, in.Permissions)
	if err != nil {
		return err
	}
	if !bitperms.CanGrant(orgPermsOf(r), grant) {
		return errEscalation
	}
	days := 7
	if in.ExpiresInDays != nil {
		days = *in.ExpiresInDays
	}
	if days < 1 || days > 30 {
		return errBadRequest("expires_in_days must be between 1 and 30")
	}
	token, hash, err := auth.NewToken(auth.InvitePrefix)
	if err != nil {
		return err
	}
	inv, err := s.store.CreateInvite(r.Context(), orgIDOf(r), email, grant, hash, p.user.ID, time.Duration(days)*24*time.Hour)
	if err != nil {
		return err
	}
	s.auditOrg(r, "invite.create", inv.ID, map[string]any{"email": email, "permissions": perms.Org.Names(grant)})
	v := inviteView(inv)
	v["token"] = token
	writeJSON(w, http.StatusCreated, map[string]any{"invite": v})
	return nil
}

func (s *Server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "invite")
	if err != nil {
		return err
	}
	if err := s.store.RevokeInvite(r.Context(), orgIDOf(r), id); err != nil {
		return err
	}
	s.auditOrg(r, "invite.revoke", id, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

var errInvite = &apiError{http.StatusNotFound, "invite_invalid", "this invite is not valid: it may have expired, been used or revoked, or be for a different account"}

func readInviteToken(r *http.Request) ([]byte, error) {
	var in struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(in.Token, auth.InvitePrefix) || len(in.Token) > 200 {
		return nil, errInvite
	}
	return auth.HashToken(in.Token), nil
}

func (s *Server) handleLookupInvite(w http.ResponseWriter, r *http.Request) error {
	hash, err := readInviteToken(r)
	if err != nil {
		return err
	}
	inv, err := s.store.InviteByToken(r.Context(), hash)
	if errors.Is(err, store.ErrNotFound) || (err == nil && inv.Email != "" && inv.Email != principalOf(r).user.Email) {
		return errInvite
	}
	if err != nil {
		return err
	}
	v := permissionView(perms.Org, inv.Perms)
	v["org_name"], v["invited_by"], v["expires_at"] = inv.OrgName, inv.InvitedBy, inv.ExpiresAt
	writeJSON(w, 200, map[string]any{"invite": v})
	return nil
}

func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) error {
	hash, err := readInviteToken(r)
	if err != nil {
		return err
	}
	u := principalOf(r).user
	inv, err := s.store.AcceptInvite(r.Context(), hash, u.ID, u.Email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errInvite
	case errors.Is(err, store.ErrConflict):
		return &apiError{http.StatusConflict, "already_member", "you are already a member of that organisation"}
	case err != nil:
		return err
	}
	s.audit(r, &inv.OrgID, "user:"+u.ID, "invite.accept", inv.ID, nil)
	writeJSON(w, 200, map[string]any{"org": map[string]string{"id": inv.OrgID, "name": inv.OrgName}})
	return nil
}

func (s *Server) handleAdminCatalogue(c interface {
	List(ctx context.Context, s *store.Store) ([]store.CatalogueItem, error)
}, key string) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		items, err := c.List(r.Context(), s.store)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{key: items, "server_image_allowlist": s.cfg.AllowedImages})
		return nil
	}
}

func (s *Server) handleAdminAddImage(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Ref         string `json:"ref"`
		Label       string `json:"label"`
		Description string `json:"description"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if !sandbox.ValidImage(in.Ref) {
		return errBadRequest("ref is not a valid image reference")
	}
	if !sandbox.ImageAllowed(s.cfg.AllowedImages, in.Ref) {
		return errBadRequest("that image is outside NALA_ALLOWED_IMAGES (%s); widen it in the server's environment first", strings.Join(s.cfg.AllowedImages, ", "))
	}
	label, err := validName("label", in.Label)
	if err != nil {
		return err
	}
	if len(in.Description) > 300 {
		return errBadRequest("description must be at most 300 characters")
	}
	return s.adminAdd(w, r, store.Images, "image", in.Ref, label, in.Description)
}

func (s *Server) handleAdminAddHost(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Pattern string `json:"pattern"`
		Label   string `json:"label"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	pattern := strings.ToLower(strings.TrimSpace(in.Pattern))
	if !egress.ValidHostPattern(pattern) {
		return errBadRequest(`pattern must be a hostname such as "api.example.com" or a subdomain wildcard such as "*.example.com"`)
	}
	label, err := validName("label", in.Label)
	if err != nil {
		return err
	}
	return s.adminAdd(w, r, store.Hosts, "host", pattern, label, "")
}

func (s *Server) adminAdd(w http.ResponseWriter, r *http.Request, c interface {
	Create(ctx context.Context, s *store.Store, value, label, description string) (string, error)
}, kind, value, label, description string) error {
	id, err := c.Create(r.Context(), s.store, value, label, description)
	if errors.Is(err, store.ErrConflict) {
		return &apiError{http.StatusConflict, "exists", "that " + kind + " is already in the list"}
	}
	if err != nil {
		return err
	}
	s.audit(r, nil, principalOf(r).actor(), "catalogue."+kind+".add", id, map[string]string{"value": value})
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	return nil
}

func (s *Server) handleAdminSetCatalogue(c interface {
	SetEnabled(ctx context.Context, s *store.Store, id string, enabled bool) error
}, kind string) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if in.Enabled == nil {
			return errBadRequest("enabled is required")
		}
		if err := c.SetEnabled(r.Context(), s.store, id, *in.Enabled); err != nil {
			return err
		}
		s.audit(r, nil, principalOf(r).actor(), "catalogue."+kind+".set", id, map[string]bool{"enabled": *in.Enabled})
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
}
