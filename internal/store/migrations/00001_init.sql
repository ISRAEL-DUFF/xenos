-- +goose Up
CREATE TABLE users (
    id                 BIGSERIAL PRIMARY KEY,
    email              TEXT NOT NULL UNIQUE,
    password_hash      TEXT NOT NULL,
    ispend_customer_id TEXT,
    email_verified_at  TIMESTAMPTZ,
    phone              TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','banned')),
    is_admin           BOOLEAN NOT NULL DEFAULT FALSE,
    vm_limit           INT NOT NULL DEFAULT 2,
    auto_convert       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash BYTEA PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL DEFAULT 'cookie' CHECK (kind IN ('cookie','bearer')),
    csrf_token TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ssh_keys (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    public_key  TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, fingerprint)
);

CREATE TABLE plans (
    id                       BIGSERIAL PRIMARY KEY,
    slug                     TEXT NOT NULL UNIQUE,
    vcpu                     INT NOT NULL,
    ram_mb                   INT NOT NULL,
    disk_gb                  INT NOT NULL,
    price_uusdt_hourly       BIGINT NOT NULL,
    price_uusdt_monthly_cap  BIGINT NOT NULL,
    active                   BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE templates (
    id                  BIGSERIAL PRIMARY KEY,
    slug                TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    proxmox_template_id INT NOT NULL UNIQUE,
    active              BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE vms (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id),
    region      TEXT NOT NULL,
    plan_id     BIGINT NOT NULL REFERENCES plans(id),
    template_id BIGINT NOT NULL REFERENCES templates(id),
    proxmox_vmid INT UNIQUE,
    hostname    TEXT NOT NULL,
    ipv4_id     BIGINT,
    ipv6        TEXT,
    state       TEXT NOT NULL DEFAULT 'pending' CHECK (state IN
        ('pending','provisioning','running','stopped','suspended','deleting','deleted','error')),
    suspended_at TIMESTAMPTZ,
    port25_unblocked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);
CREATE INDEX vms_user_idx ON vms(user_id) WHERE deleted_at IS NULL;

CREATE TABLE ip_addresses (
    id      BIGSERIAL PRIMARY KEY,
    address INET NOT NULL UNIQUE,
    gateway INET NOT NULL,
    region  TEXT NOT NULL,
    vm_id   BIGINT REFERENCES vms(id)
);
ALTER TABLE vms ADD CONSTRAINT vms_ipv4_fk FOREIGN KEY (ipv4_id) REFERENCES ip_addresses(id);

CREATE TABLE jobs (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT NOT NULL,
    payload    JSONB NOT NULL DEFAULT '{}',
    status     TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','done','failed')),
    attempts   INT NOT NULL DEFAULT 0,
    run_after  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX jobs_ready_idx ON jobs(run_after) WHERE status = 'queued';

CREATE TABLE usage_charges (
    id                 BIGSERIAL PRIMARY KEY,
    user_id            BIGINT NOT NULL REFERENCES users(id),
    vm_id              BIGINT NOT NULL REFERENCES vms(id),
    hour               TIMESTAMPTZ NOT NULL,
    amount_uusdt       BIGINT NOT NULL,
    ispend_movement_id TEXT,
    status             TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','paid','unpaid','refunded')),
    UNIQUE (vm_id, hour)
);

CREATE TABLE conversions (
    id               BIGSERIAL PRIMARY KEY,
    user_id          BIGINT NOT NULL REFERENCES users(id),
    ispend_quote_id  TEXT,
    amount_ngn_kobo  BIGINT NOT NULL,
    amount_uusdt     BIGINT,
    rate             NUMERIC,
    ispend_movement_id TEXT,
    deposit_event_id TEXT UNIQUE,
    status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','complete','failed')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE webhook_events (
    id          TEXT PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO plans (slug, vcpu, ram_mb, disk_gb, price_uusdt_hourly, price_uusdt_monthly_cap) VALUES
    ('nano',   1, 1024, 20,  6000, 6000 * 730),
    ('small',  2, 2048, 40, 12000, 12000 * 730),
    ('medium', 2, 4096, 80, 24000, 24000 * 730);
INSERT INTO templates (slug, name, proxmox_template_id) VALUES
    ('ubuntu-24.04', 'Ubuntu 24.04', 9000),
    ('debian-12',    'Debian 12',    9001);

-- +goose Down
DROP TABLE webhook_events, conversions, usage_charges, jobs, ip_addresses, vms, templates, plans, ssh_keys, sessions, users CASCADE;
