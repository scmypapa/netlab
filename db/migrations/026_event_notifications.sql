CREATE FUNCTION notify_environment_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.environment_id IS NOT NULL THEN
  PERFORM pg_notify('netlab_events', NEW.environment_id);
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER environment_event_added AFTER INSERT ON events
 FOR EACH ROW EXECUTE FUNCTION notify_environment_event();
