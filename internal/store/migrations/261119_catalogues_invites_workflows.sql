UPDATE memberships SET perms = CASE perms
    WHEN 4095 THEN 1048575
    WHEN 255 THEN 459007
    WHEN 175 THEN 458927
    WHEN 161 THEN 65697
    ELSE perms END;
UPDATE users SET platform_perms = 7 WHERE platform_perms = 3;

CREATE TABLE images (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ref text NOT NULL UNIQUE,
    label text NOT NULL,
    description text NOT NULL DEFAULT '',
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE org_images (
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    image_id uuid NOT NULL REFERENCES images (id) ON DELETE CASCADE,
    PRIMARY KEY (org_id, image_id)
);

CREATE TABLE hosts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    pattern text NOT NULL UNIQUE CHECK (pattern = lower(pattern)),
    label text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE org_hosts (
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    host_id uuid NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    PRIMARY KEY (org_id, host_id)
);

INSERT INTO images (ref, label, description) VALUES
    ('alpine:3.20', 'Alpine 3.20', 'Minimal Linux with a shell and core utilities'),
    ('busybox:1.36', 'BusyBox 1.36', 'The smallest shell environment'),
    ('python:3.12-alpine', 'Python 3.12', 'Python on Alpine'),
    ('node:22-alpine', 'Node.js 22', 'Node.js on Alpine');

INSERT INTO hosts (pattern, label) VALUES
    ('github.com', 'GitHub'),
    ('api.github.com', 'GitHub API'),
    ('*.githubusercontent.com', 'GitHub raw content'),
    ('registry.npmjs.org', 'npm registry'),
    ('pypi.org', 'PyPI'),
    ('files.pythonhosted.org', 'PyPI downloads'),
    ('proxy.golang.org', 'Go module proxy');

INSERT INTO org_images (org_id, image_id) SELECT o.id, i.id FROM orgs o CROSS JOIN images i;

ALTER TABLE orgs
    ADD COLUMN allow_network boolean NOT NULL DEFAULT false,
    ADD COLUMN max_timeout_seconds int NOT NULL DEFAULT 3600 CHECK (max_timeout_seconds >= 1),
    ADD COLUMN max_attempts int NOT NULL DEFAULT 20 CHECK (max_attempts BETWEEN 1 AND 20),
    ADD COLUMN max_running int NOT NULL DEFAULT 10 CHECK (max_running >= 0),
    ADD COLUMN max_pending int NOT NULL DEFAULT 1000 CHECK (max_pending >= 0);

CREATE TABLE invites (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    email text NOT NULL DEFAULT '',
    perms bigint NOT NULL CHECK (perms >= 0),
    token_hash bytea NOT NULL UNIQUE,
    created_by uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    accepted_by uuid REFERENCES users (id) ON DELETE SET NULL,
    revoked_at timestamptz
);
CREATE INDEX invites_org_idx ON invites (org_id, created_at DESC);

CREATE TABLE workflows (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name text NOT NULL,
    graph jsonb NOT NULL,
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX workflows_org_idx ON workflows (org_id, updated_at DESC);

CREATE TABLE workflow_runs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    workflow_id uuid REFERENCES workflows (id) ON DELETE SET NULL,
    name text NOT NULL,
    graph jsonb NOT NULL,
    status text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX workflow_runs_org_idx ON workflow_runs (org_id, created_at DESC);
CREATE INDEX workflow_runs_live_idx ON workflow_runs (id) WHERE status = 'running';

ALTER TABLE jobs
    ADD COLUMN name text NOT NULL DEFAULT '',
    ADD COLUMN network text NOT NULL DEFAULT 'none' CHECK (network IN ('none', 'egress')),
    ADD COLUMN run_id uuid REFERENCES workflow_runs (id) ON DELETE CASCADE,
    ADD COLUMN node_id text,
    ADD COLUMN proxy_token_hash bytea;

ALTER TABLE jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check
    CHECK (status IN ('pending', 'blocked', 'running', 'succeeded', 'dead', 'cancelled', 'skipped'));

CREATE INDEX jobs_run_idx ON jobs (run_id) WHERE run_id IS NOT NULL;
CREATE UNIQUE INDEX jobs_proxy_token_idx ON jobs (proxy_token_hash) WHERE proxy_token_hash IS NOT NULL;
CREATE INDEX jobs_org_running_idx ON jobs (org_id) WHERE status = 'running';

CREATE TABLE job_deps (
    job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    parent_id uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    condition text NOT NULL CHECK (condition IN ('success', 'failure', 'always')),
    PRIMARY KEY (job_id, parent_id)
);
CREATE INDEX job_deps_parent_idx ON job_deps (parent_id);
