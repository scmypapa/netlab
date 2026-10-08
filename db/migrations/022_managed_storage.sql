ALTER TABLE storage_pools ADD COLUMN managed boolean NOT NULL DEFAULT false;
ALTER TABLE storage_pools DROP CONSTRAINT storage_pools_state_check;
ALTER TABLE storage_pools ADD CHECK (state IN ('ready','preparing','deleting'));
CREATE UNIQUE INDEX storage_managed_cluster ON storage_pools(managed) WHERE managed;
