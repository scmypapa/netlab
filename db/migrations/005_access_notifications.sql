CREATE FUNCTION notify_access_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE subject text;
BEGIN
  IF TG_TABLE_NAME = 'principals' THEN
    subject := OLD.id;
  ELSE
    subject := OLD.principal_id;
  END IF;
  PERFORM pg_notify('netlab_access', subject);
  RETURN NULL;
END;
$$;

CREATE TRIGGER principal_access_changed AFTER UPDATE OF disabled, password_hash ON principals
  FOR EACH ROW EXECUTE FUNCTION notify_access_change();
CREATE TRIGGER credential_access_changed AFTER DELETE ON credentials
  FOR EACH ROW EXECUTE FUNCTION notify_access_change();
CREATE TRIGGER grant_access_changed AFTER UPDATE OR DELETE ON grants
  FOR EACH ROW EXECUTE FUNCTION notify_access_change();
