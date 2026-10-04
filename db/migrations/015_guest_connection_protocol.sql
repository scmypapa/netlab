ALTER TABLE guest_connections ADD COLUMN protocol text NOT NULL DEFAULT 'ssh';
ALTER TABLE guest_connections DROP CONSTRAINT guest_connections_pkey;
ALTER TABLE guest_connections ADD PRIMARY KEY (principal_id,environment_id,asset_id,protocol);
