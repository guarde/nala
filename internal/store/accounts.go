package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"guarde.sh/nala/pkg/bitperms"
)

type User struct {
	ID            string        `json:"id"`
	Email         string        `json:"email"`
	PasswordHash  string        `json:"-"`
	PlatformPerms bitperms.Perm `json:"-"`
	Disabled      bool          `json:"disabled"`
	CreatedAt     time.Time     `json:"created_at"`
}

const userCols = `u.id, u.email, u.password_hash, u.platform_perms, u.disabled, u.created_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var pp int64
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &pp, &u.Disabled, &u.CreatedAt); err != nil {
		return u, notFound(err)
	}
	var err error
	u.PlatformPerms, err = bitperms.FromInt64(pp)
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, email, hash string, platform bitperms.Perm) (User, error) {
	u, err := scanUser(s.DB.QueryRowContext(ctx,
		`INSERT INTO users AS u (email, password_hash, platform_perms) VALUES ($1, $2, $3) RETURNING `+userCols,
		email, hash, platform.Int64()))
	if isUniqueViolation(err) {
		return u, ErrConflict
	}
	return u, err
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.email = $1`, email))
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.id = $1`, id))
}

func (s *Store) ListUsers(ctx context.Context, limit, offset int) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+userCols+` FROM users u ORDER BY u.created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) SetPassword(ctx context.Context, userID, hash string) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash))
}

func (s *Store) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := affected(tx.ExecContext(ctx, `UPDATE users SET disabled = $2 WHERE id = $1`, userID, disabled)); err != nil {
			return err
		}
		if disabled {
			_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
			return err
		}
		return nil
	})
}

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID string, ttl time.Duration, ip, ua string) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at, ip, user_agent)
		 VALUES ($1, $2, now() + make_interval(secs => $3), $4, $5)`,
		tokenHash, userID, ttl.Seconds(), ip, ua)
	return err
}

func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (User, error) {
	return scanUser(s.DB.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1 AND s.expires_at > now() AND NOT u.disabled`, tokenHash))
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	return err
}

type Org struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type OrgMembership struct {
	Org
	Perms bitperms.Perm `json:"-"`
}

type Member struct {
	UserID    string        `json:"user_id"`
	Email     string        `json:"email"`
	Perms     bitperms.Perm `json:"-"`
	CreatedAt time.Time     `json:"joined_at"`
}

func (s *Store) CreateOrg(ctx context.Context, name, ownerID string, ownerPerms bitperms.Perm) (Org, error) {
	var o Org
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `INSERT INTO orgs (name) VALUES ($1) RETURNING id, name, created_at`, name).
			Scan(&o.ID, &o.Name, &o.CreatedAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO memberships (org_id, user_id, perms) VALUES ($1, $2, $3)`, o.ID, ownerID, ownerPerms.Int64())
		return err
	})
	return o, err
}

func (s *Store) RenameOrg(ctx context.Context, orgID, name string) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE orgs SET name = $2 WHERE id = $1`, orgID, name))
}

func (s *Store) OrgsForUser(ctx context.Context, userID string) ([]OrgMembership, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT o.id, o.name, o.created_at, m.perms FROM memberships m JOIN orgs o ON o.id = m.org_id
		 WHERE m.user_id = $1 ORDER BY o.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrgMembership{}
	for rows.Next() {
		var m OrgMembership
		var p int64
		if err := rows.Scan(&m.ID, &m.Name, &m.CreatedAt, &p); err != nil {
			return nil, err
		}
		m.Perms = bitperms.Perm(p)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListOrgs(ctx context.Context, limit, offset int) ([]Org, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, created_at FROM orgs ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Org{}
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) MembershipPerms(ctx context.Context, orgID, userID string) (bitperms.Perm, error) {
	var p int64
	err := s.DB.QueryRowContext(ctx, `SELECT perms FROM memberships WHERE org_id = $1 AND user_id = $2`, orgID, userID).Scan(&p)
	if err != nil {
		return bitperms.None, notFound(err)
	}
	return bitperms.FromInt64(p)
}

func (s *Store) ListMembers(ctx context.Context, orgID string) ([]Member, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT u.id, u.email, m.perms, m.created_at FROM memberships m JOIN users u ON u.id = m.user_id
		 WHERE m.org_id = $1 ORDER BY m.created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		var p int64
		if err := rows.Scan(&m.UserID, &m.Email, &p, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Perms = bitperms.Perm(p)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) AddMember(ctx context.Context, orgID, userID string, p bitperms.Perm) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO memberships (org_id, user_id, perms) VALUES ($1, $2, $3)`, orgID, userID, p.Int64())
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) ChangeMember(ctx context.Context, orgID, userID string, newPerms *bitperms.Perm, ownerMask bitperms.Perm) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE`, orgID); err != nil {
			return err
		}
		var err error
		if newPerms == nil {
			err = affected(tx.ExecContext(ctx, `DELETE FROM memberships WHERE org_id = $1 AND user_id = $2`, orgID, userID))
		} else {
			err = affected(tx.ExecContext(ctx, `UPDATE memberships SET perms = $3 WHERE org_id = $1 AND user_id = $2`, orgID, userID, newPerms.Int64()))
		}
		if err != nil {
			return err
		}
		var owners int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memberships WHERE org_id = $1 AND perms & $2 = $2`, orgID, ownerMask.Int64()).Scan(&owners); err != nil {
			return err
		}
		if owners == 0 {
			return ErrLastOwner
		}
		return nil
	})
}

type APIKey struct {
	ID         string        `json:"id"`
	OrgID      string        `json:"org_id"`
	Name       string        `json:"name"`
	Hint       string        `json:"hint"`
	Perms      bitperms.Perm `json:"-"`
	CreatedAt  time.Time     `json:"created_at"`
	ExpiresAt  *time.Time    `json:"expires_at"`
	LastUsedAt *time.Time    `json:"last_used_at"`
	RevokedAt  *time.Time    `json:"revoked_at"`
}

const keyCols = `id, org_id, name, hint, perms, created_at, expires_at, last_used_at, revoked_at`

func scanKey(row interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	var p int64
	if err := row.Scan(&k.ID, &k.OrgID, &k.Name, &k.Hint, &p, &k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
		return k, notFound(err)
	}
	var err error
	k.Perms, err = bitperms.FromInt64(p)
	return k, err
}

func (s *Store) CreateAPIKey(ctx context.Context, orgID, name string, hash []byte, hint string, p bitperms.Perm, createdBy string, expiresAt *time.Time) (APIKey, error) {
	return scanKey(s.DB.QueryRowContext(ctx,
		`INSERT INTO api_keys (org_id, name, key_hash, hint, perms, created_by, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+keyCols,
		orgID, name, hash, hint, p.Int64(), createdBy, expiresAt))
}

func (s *Store) APIKeyByHash(ctx context.Context, hash []byte) (APIKey, error) {
	return scanKey(s.DB.QueryRowContext(ctx,
		`UPDATE api_keys SET last_used_at = now()
		 WHERE key_hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
		 RETURNING `+keyCols, hash))
}

func (s *Store) ListAPIKeys(ctx context.Context, orgID string) ([]APIKey, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeAPIKey(ctx context.Context, orgID, keyID string) error {
	return affected(s.DB.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE org_id = $1 AND id = $2 AND revoked_at IS NULL`, orgID, keyID))
}

type AuditEntry struct {
	ID     int64           `json:"id"`
	TS     time.Time       `json:"ts"`
	OrgID  *string         `json:"org_id"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"`
	IP     string          `json:"ip"`
}

func (s *Store) Audit(ctx context.Context, orgID *string, actor, action, target string, detail any, ip string) error {
	d := []byte("{}")
	if detail != nil {
		var err error
		if d, err = json.Marshal(detail); err != nil {
			return err
		}
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO audit_log (org_id, actor, action, target, detail, ip) VALUES ($1, $2, $3, $4, $5, $6)`,
		orgID, actor, action, target, d, ip)
	return err
}

func (s *Store) ListAudit(ctx context.Context, orgID *string, limit, offset int) ([]AuditEntry, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, ts, org_id, actor, action, target, detail, ip FROM audit_log
		 WHERE ($1::uuid IS NULL OR org_id = $1::uuid) ORDER BY id DESC LIMIT $2 OFFSET $3`, orgID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var d []byte
		if err := rows.Scan(&e.ID, &e.TS, &e.OrgID, &e.Actor, &e.Action, &e.Target, &d, &e.IP); err != nil {
			return nil, err
		}
		e.Detail = d
		out = append(out, e)
	}
	return out, rows.Err()
}
