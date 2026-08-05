-- Full schema for the VM lifecycle (zones, nodes, IP pools, IPs, storage
-- types, images, vms). Applied after 0001_init.sql.
--
-- The vms and ips tables reference each other (vms.ip_id -> ips.id and
-- ips.vm_id -> vms.id); both columns are nullable. The cycle is resolved by
-- creating vms first (without the ip_id FK), then ips (referencing vms), and
-- finally adding vms.ip_id's FK via ALTER TABLE.
--
-- Repository-layer conventions:
--
-- * IP allocation write order (atomic reservation within the FK cycle):
--   inside a single transaction, execute:
--     1. INSERT vms with ip_id temporarily NULL;
--     2. UPDATE ips SET status='used', vm_id=$vms_id
--        WHERE id=$ip_id AND status='free'
--        (if affected rows = 0, roll back and pick another IP);
--     3. UPDATE vms SET ip_id=$ip_id.
--   This keeps the ips.vm_id FK valid at every step and makes the
--   free->used claim atomic without a lost-update window.
--
-- * Destroy/release semantics: ips.vm_id has ON DELETE SET NULL, which only
--   clears the reference and does NOT reset status to 'free'. A repository
--   destroy flow must, in the same transaction, run
--     UPDATE ips SET status='free', vm_id=NULL WHERE vm_id=$1
--   BEFORE deleting the vms row, otherwise orphan IPs with status='used'
--   and no owner are left behind.
--
-- * When inserting into images, the NodeImages field must be initialized to
--   a non-nil map so the JSONB column is written as '{}' consistently with
--   the column default, never as SQL NULL.

CREATE TABLE zones (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE pve_nodes (
    id               BIGSERIAL PRIMARY KEY,
    zone_id          BIGINT NOT NULL REFERENCES zones(id),
    name             TEXT NOT NULL,
    host             TEXT NOT NULL,
    api_user         TEXT NOT NULL,
    api_token_secret TEXT NOT NULL,
    enabled          BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ip_pools (
    id           BIGSERIAL PRIMARY KEY,
    zone_id      BIGINT NOT NULL REFERENCES zones(id),
    name         TEXT NOT NULL,
    network_cidr TEXT NOT NULL,
    gateway      TEXT NOT NULL,
    dns          TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ip_pool_nodes (
    ip_pool_id BIGINT NOT NULL REFERENCES ip_pools(id),
    node_id    BIGINT NOT NULL REFERENCES pve_nodes(id),
    PRIMARY KEY (ip_pool_id, node_id)
);

CREATE TABLE storage_types (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    display_name TEXT NOT NULL,
    pve_storage  TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE images (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    default_user TEXT NOT NULL,
    node_images  JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE vms (
    id                 BIGSERIAL PRIMARY KEY,
    uuid               TEXT NOT NULL UNIQUE,
    name               TEXT NOT NULL,
    zone_id            BIGINT NOT NULL REFERENCES zones(id),
    node_id            BIGINT NOT NULL REFERENCES pve_nodes(id),
    pve_vmid           BIGINT NOT NULL,
    image_id           BIGINT NOT NULL REFERENCES images(id),
    storage_type_id    BIGINT NOT NULL REFERENCES storage_types(id),
    cpu                INTEGER NOT NULL,
    mem_mb             BIGINT NOT NULL,
    disk_gb            BIGINT NOT NULL,
    ip_id              BIGINT,
    password_encrypted TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ips (
    id         BIGSERIAL PRIMARY KEY,
    pool_id    BIGINT NOT NULL REFERENCES ip_pools(id),
    ip         TEXT NOT NULL UNIQUE,
    status     TEXT NOT NULL DEFAULT 'free',
    vm_id      BIGINT REFERENCES vms(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ips_status ON ips (status);

-- Resolve the vms <-> ips circular dependency (see header comment).
ALTER TABLE vms
    ADD CONSTRAINT vms_ip_id_fkey
    FOREIGN KEY (ip_id) REFERENCES ips(id)
    ON DELETE SET NULL;
