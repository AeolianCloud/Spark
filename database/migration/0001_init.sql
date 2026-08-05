-- Minimal bootstrap migration; full schema (zones, vms, etc.) lands in 2.2.
CREATE TABLE IF NOT EXISTS schema_probe (
    id integer PRIMARY KEY
);
