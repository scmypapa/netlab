CREATE INDEX environments_created_cursor ON environments(created_at DESC,id DESC) WHERE status<>'destroyed';
CREATE INDEX templates_created_cursor ON templates(created_at DESC,id DESC);
CREATE INDEX blueprints_created_cursor ON blueprints(created_at DESC,id DESC);
