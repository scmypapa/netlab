ALTER TABLE nodes ADD COLUMN memory_reserve_mib bigint NOT NULL DEFAULT 0 CHECK (memory_reserve_mib >= 0);
