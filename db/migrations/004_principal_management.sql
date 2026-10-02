ALTER TABLE principals ADD COLUMN disabled boolean NOT NULL DEFAULT false;
ALTER TABLE principals ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX principals_kind_cursor ON principals(kind,id);
