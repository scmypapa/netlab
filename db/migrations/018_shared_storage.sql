ALTER TABLE storage_pools
  ADD COLUMN node_ids text[],
  ADD COLUMN driver text NOT NULL DEFAULT 'directory' CHECK (driver IN ('directory','rbd'));

UPDATE storage_pools SET node_ids=ARRAY[node_id];
ALTER TABLE storage_pools ALTER COLUMN node_ids SET NOT NULL;
ALTER TABLE storage_pools ADD CHECK (cardinality(node_ids)>0);
ALTER TABLE storage_pools DROP CONSTRAINT storage_pools_node_id_directory_key;
ALTER TABLE storage_pools DROP COLUMN node_id;
CREATE UNIQUE INDEX storage_pool_directory ON storage_pools(node_ids,directory) WHERE driver='directory';
