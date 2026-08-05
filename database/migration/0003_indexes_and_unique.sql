-- Uniqueness constraints and hot-path indexes for the VM lifecycle.
-- Applied after 0002_create_tables.sql.
--
-- * storage_types.name and images.name become unique so duplicate-name
--   inserts are refused by the database (repository maps SQLSTATE 23505 to
--   ErrConflict) instead of relying only on best-effort service scans.
-- * ips gets a composite (pool_id, status) index: the allocation SQL
--   (repository selectFreeIPSQL) filters on pool_id AND status='free', which
--   a composite index serves far better than a standalone status index.
--   idx_ips_status from 0002 is kept on purpose: it is cheap to maintain and
--   still serves scans that filter on status alone.
--
-- The vms, ips and node/zone tables already have PK/FK/unique indexes from
-- 0002; this migration only adds what 0002 omitted.

CREATE UNIQUE INDEX storage_types_name_key ON storage_types(name);
CREATE UNIQUE INDEX images_name_key ON images(name);
CREATE INDEX idx_ips_pool_status ON ips(pool_id, status);
