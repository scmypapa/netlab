-- Reapply existing networks through the same durable operation queue. Assets,
-- service leases, VPN identities and drafts are kept in place.
INSERT INTO operations(id,environment_id,scope_kind,scope_id,kind,payload,expected_revision)
SELECT gen_random_uuid()::text,e.id,'environment',e.id,'refresh-network','{}',e.revision
FROM environments e
WHERE e.applied_spec IS NOT NULL AND e.network_node_id IS NOT NULL
  AND e.status<>'destroyed' AND jsonb_array_length(e.applied_spec->'networks')>0
  AND NOT EXISTS (SELECT 1 FROM operations pending WHERE pending.id=e.operation_id
    AND pending.state IN ('failed','partially_applied')
    AND pending.phase NOT IN ('rolled-back','complete','cancelled'));
