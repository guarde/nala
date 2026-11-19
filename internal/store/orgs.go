package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
	"guarde.sh/nala/pkg/bitperms"
)

type OrgSettings struct {
	AllowNetwork      bool `json:"allow_network"`
	MaxTimeoutSeconds int  `json:"max_timeout_seconds"`
	MaxAttempts       int  `json:"max_attempts"`
	MaxRunning        int  `json:"max_running"`
	MaxPending        int  `json:"max_pending"`
}

type OrgDetail struct {
	Org
	Settings OrgSettings `json:"settings"`
	Members  int         `json:"members"`
}

func (s *Store) GetOrg(ctx context.Context, id string) (OrgDetail, error) {
	var o OrgDetail
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, name, created_at, allow_network, max_timeout_seconds, max_attempts, max_running, max_pending,
		   (SELECT count(*) FROM memberships m WHERE m.org_id = o.id)
		 FROM orgs o WHERE id = $1`, id).
		Scan(&o.ID, &o.Name, &o.CreatedAt, &o.Settings.AllowNetwork, &o.Settings.MaxTimeoutSeconds,
			&o.Settings.MaxAttempts, &o.Settings.MaxRunning, &o.Settings.MaxPending, &o.Members)
	return o, notFound(err)
}

func (s *Store) UpdateOrgSettings(ctx context.Context, id string, st OrgSettings) error {
	return affected(s.DB.ExecContext(ctx,
		`UPDATE orgs SET allow_network = $2, max_timeout_seconds = $3, max_attempts = $4, max_running = $5, max_pending = $6 WHERE id = $1`,
		id, st.AllowNetwork, st.MaxTimeoutSeconds, st.MaxAttempts, st.MaxRunning, st.MaxPending))
}

func (s *Store) DeleteOrg(ctx context.Context, id string) error {
	return affected(s.DB.ExecContext(ctx, `DELETE FROM orgs WHERE id = $1`, id))
}

type CatalogueItem struct {
	ID          string    `json:"id"`
	Value       string    `json:"value"`
	Label       string    `json:"label"`
	Description string    `json:"description,omitempty"`
	Enabled     bool      `json:"enabled"`
	OrgEnabled  bool      `json:"org_enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

type catalogue struct{ table, value, link, fk, desc string }

var (
	Images = catalogue{"images", "ref", "org_images", "image_id", "description"}
	Hosts  = catalogue{"hosts", "pattern", "org_hosts", "host_id", "''"}
)

func (c catalogue) List(ctx context.Context, s *Store) ([]CatalogueItem, error) {
	return c.query(ctx, s, `SELECT t.id, t.`+c.value+`, t.label, `+c.desc+`, t.enabled, false, t.created_at FROM `+c.table+` t ORDER BY t.label`)
}

func (c catalogue) ForOrg(ctx context.Context, s *Store, orgID string) ([]CatalogueItem, error) {
	return c.query(ctx, s,
		`SELECT t.id, t.`+c.value+`, t.label, `+c.desc+`, t.enabled,
		   EXISTS (SELECT 1 FROM `+c.link+` l WHERE l.org_id = $1 AND l.`+c.fk+` = t.id), t.created_at
		 FROM `+c.table+` t WHERE t.enabled ORDER BY t.label`, orgID)
}

func (c catalogue) query(ctx context.Context, s *Store, q string, args ...any) ([]CatalogueItem, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogueItem{}
	for rows.Next() {
		var it CatalogueItem
		if err := rows.Scan(&it.ID, &it.Value, &it.Label, &it.Description, &it.Enabled, &it.OrgEnabled, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (c catalogue) Create(ctx context.Context, s *Store, value, label, description string) (string, error) {
	var id string
	var err error
	if c.table == "images" {
		err = s.DB.QueryRowContext(ctx, `INSERT INTO images (ref, label, description) VALUES ($1, $2, $3) RETURNING id`, value, label, description).Scan(&id)
	} else {
		err = s.DB.QueryRowContext(ctx, `INSERT INTO hosts (pattern, label) VALUES ($1, $2) RETURNING id`, value, label).Scan(&id)
	}
	if isUniqueViolation(err) {
		return "", ErrConflict
	}
	return id, err
}

func (c catalogue) SetEnabled(ctx context.Context, s *Store, id string, enabled bool) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE `+c.table+` SET enabled = $2 WHERE id = $1`, id, enabled))
}

func (c catalogue) SetForOrg(ctx context.Context, s *Store, orgID, id string, enabled bool) error {
	if !enabled {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM `+c.link+` WHERE org_id = $1 AND `+c.fk+` = $2`, orgID, id)
		return err
	}
	var offered bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+c.table+` WHERE id = $1 AND enabled)`, id).Scan(&offered); err != nil {
		return err
	}
	if !offered {
		return ErrNotFound
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO `+c.link+` (org_id, `+c.fk+`) VALUES ($1, $2) ON CONFLICT DO NOTHING`, orgID, id)
	return err
}

func (s *Store) OrgImageAllowed(ctx context.Context, orgID, ref string) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM org_images l JOIN images i ON i.id = l.image_id
		                WHERE l.org_id = $1 AND i.ref = $2 AND i.enabled)`, orgID, ref).Scan(&ok)
	return ok, err
}

type EgressGrant struct {
	JobID        string
	Attempt      int
	OrgID        string
	AllowNetwork bool
	Patterns     []string
}

func (s *Store) EgressGrantByToken(ctx context.Context, tokenHash []byte) (EgressGrant, error) {
	var g EgressGrant
	err := s.DB.QueryRowContext(ctx,
		`SELECT j.id, j.attempts, j.org_id, o.allow_network,
		   COALESCE(array_agg(h.pattern) FILTER (WHERE h.id IS NOT NULL), '{}')
		 FROM jobs j
		 JOIN orgs o ON o.id = j.org_id
		 LEFT JOIN org_hosts l ON l.org_id = o.id
		 LEFT JOIN hosts h ON h.id = l.host_id AND h.enabled
		 WHERE j.proxy_token_hash = $1 AND j.status = 'running' AND j.network = 'egress'
		 GROUP BY j.id, o.allow_network`, tokenHash).
		Scan(&g.JobID, &g.Attempt, &g.OrgID, &g.AllowNetwork, pq.Array(&g.Patterns))
	return g, notFound(err)
}

type Invite struct {
	ID         string        `json:"id"`
	OrgID      string        `json:"org_id"`
	OrgName    string        `json:"org_name"`
	Email      string        `json:"email"`
	Perms      bitperms.Perm `json:"-"`
	CreatedBy  string        `json:"created_by"`
	InvitedBy  string        `json:"invited_by"`
	CreatedAt  time.Time     `json:"created_at"`
	ExpiresAt  time.Time     `json:"expires_at"`
	AcceptedAt *time.Time    `json:"accepted_at"`
	RevokedAt  *time.Time    `json:"revoked_at"`
}

const inviteCols = `i.id, i.org_id, o.name, i.email, i.perms, i.created_by, u.email, i.created_at, i.expires_at, i.accepted_at, i.revoked_at`
const inviteFrom = ` FROM invites i JOIN orgs o ON o.id = i.org_id JOIN users u ON u.id = i.created_by`
const inviteOpen = ` WHERE i.token_hash = $1 AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()`

func scanInvite(row interface{ Scan(...any) error }) (Invite, error) {
	var i Invite
	var p int64
	if err := row.Scan(&i.ID, &i.OrgID, &i.OrgName, &i.Email, &p, &i.CreatedBy, &i.InvitedBy, &i.CreatedAt, &i.ExpiresAt, &i.AcceptedAt, &i.RevokedAt); err != nil {
		return i, notFound(err)
	}
	var err error
	i.Perms, err = bitperms.FromInt64(p)
	return i, err
}

func (s *Store) CreateInvite(ctx context.Context, orgID, email string, p bitperms.Perm, tokenHash []byte, createdBy string, ttl time.Duration) (Invite, error) {
	var id string
	err := s.DB.QueryRowContext(ctx,
		`INSERT INTO invites (org_id, email, perms, token_hash, created_by, expires_at)
		 VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6)) RETURNING id`,
		orgID, email, p.Int64(), tokenHash, createdBy, ttl.Seconds()).Scan(&id)
	if err != nil {
		return Invite{}, err
	}
	return scanInvite(s.DB.QueryRowContext(ctx, `SELECT `+inviteCols+inviteFrom+` WHERE i.id = $1`, id))
}

func (s *Store) ListInvites(ctx context.Context, orgID string) ([]Invite, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+inviteCols+inviteFrom+`
		 WHERE i.org_id = $1 AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()
		 ORDER BY i.created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) RevokeInvite(ctx context.Context, orgID, id string) error {
	return affected(s.DB.ExecContext(ctx,
		`UPDATE invites SET revoked_at = now() WHERE org_id = $1 AND id = $2 AND accepted_at IS NULL AND revoked_at IS NULL`, orgID, id))
}

func (s *Store) InviteByToken(ctx context.Context, tokenHash []byte) (Invite, error) {
	return scanInvite(s.DB.QueryRowContext(ctx, `SELECT `+inviteCols+inviteFrom+inviteOpen, tokenHash))
}

func (s *Store) AcceptInvite(ctx context.Context, tokenHash []byte, userID, userEmail string) (Invite, error) {
	var inv Invite
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		inv, err = scanInvite(tx.QueryRowContext(ctx, `SELECT `+inviteCols+inviteFrom+inviteOpen+` FOR UPDATE OF i`, tokenHash))
		if err != nil {
			return err
		}
		if inv.Email != "" && inv.Email != userEmail {
			return ErrNotFound
		}
		var raw int64
		err = tx.QueryRowContext(ctx, `SELECT perms FROM memberships WHERE org_id = $1 AND user_id = $2`, inv.OrgID, inv.CreatedBy).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		inviter, err := bitperms.FromInt64(raw)
		if err != nil {
			return err
		}
		if !bitperms.CanGrant(inviter, inv.Perms) {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (org_id, user_id, perms) VALUES ($1, $2, $3)`, inv.OrgID, userID, inv.Perms.Int64()); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE invites SET accepted_at = now(), accepted_by = $2 WHERE id = $1`, inv.ID, userID)
		return err
	})
	return inv, err
}
