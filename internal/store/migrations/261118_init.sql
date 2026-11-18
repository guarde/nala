CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email text NOT NULL UNIQUE CHECK (email = lower(email)),
    password_hash text NOT NULL,
    platform_perms bigint NOT NULL DEFAULT 0 CHECK (platform_perms >= 0),
    disabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash bytea PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    ip text NOT NULL DEFAULT '',
    user_agent text NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

CREATE TABLE orgs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    perms bigint NOT NULL CHECK (perms >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX memberships_user_idx ON memberships (user_id);

CREATE TABLE api_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name text NOT NULL,
    key_hash bytea NOT NULL UNIQUE,
    hint text NOT NULL,
    perms bigint NOT NULL CHECK (perms >= 0),
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    last_used_at timestamptz,
    revoked_at timestamptz
);
CREATE INDEX api_keys_org_idx ON api_keys (org_id);

CREATE TABLE queues (
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name text NOT NULL,
    paused boolean NOT NULL DEFAULT false,
    PRIMARY KEY (org_id, name)
);

CREATE TABLE jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    queue text NOT NULL DEFAULT 'default',
    image text NOT NULL,
    command text[] NOT NULL DEFAULT '{}',
    env jsonb NOT NULL DEFAULT '{}',
    status text NOT NULL DEFAULT 'pending' CHECK (status in ('pending', 'running', 'succeeded', 'dead', 'cancelled')),
    priority int NOT NULL DEFAULT 0,
    attempts int NOT NULL DEFAULT 0,
    max_attempts int NOT NULL DEFAULT 3,
    timeout_seconds int NOT NULL DEFAULT 300,
    run_at timestamptz NOT NULL DEFAULT now(),
    locked_by text,
    locked_until timestamptz,
    cancel_requested boolean NOT NULL DEFAULT false,
    last_error text,
    exit_code int,
    idempotency_key text,
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz
);

CREATE INDEX jobs_claim_idx ON jobs (priority DESC, run_at, created_at) WHERE status = 'pending';
CREATE INDEX jobs_lease_idx ON jobs (locked_until) WHERE status = 'running';
CREATE INDEX jobs_org_idx ON jobs (org_id, created_at DESC);
CREATE UNIQUE INDEX jobs_idempotency_idx ON jobs (org_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE job_logs (
    -- mmmm cereal
    id bigserial PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    attempt int NOT NULL,
    stream text NOT NULL CHECK (stream IN ('stdout', 'stderr', 'system')),
    line text NOT NULL,
    ts timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX job_logs_job_idx ON job_logs (job_id, id);

CREATE TABLE workers (
    id text PRIMARY KEY,
    hostname text NOT NULL,
    concurrency int NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(), 
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    stopped_at timestamptz
);

CREATE TABLE audit_log (
    id bigserial PRIMARY KEY,
    ts timestamptz NOT NULL DEFAULT now(),
    org_id uuid,
    actor text NOT NULL,
    action text NOT NULL,
    target text NOT NULL DEFAULT '',
    detail jsonb NOT NULL DEFAULT '{}',
    ip text NOT NULL DEFAULT ''
);
CREATE INDEX audit_org_idx ON audit_log (org_id, id DESC);