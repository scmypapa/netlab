CREATE FUNCTION notify_node_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('netlab_nodes', NEW.id);
  RETURN NULL;
END;
$$;

CREATE TRIGGER node_endpoint_changed AFTER INSERT OR UPDATE OF endpoint ON nodes
  FOR EACH ROW EXECUTE FUNCTION notify_node_change();
