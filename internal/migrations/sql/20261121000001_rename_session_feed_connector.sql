-- The session feed connector was named skipper. Its settings row, with its
-- settings, secrets, last test, and version, and any state the source kept
-- move to the new name. admin_audit_log keeps the old name: each entry
-- records what was done under the name it had.
UPDATE connector_settings SET connector = 'session_feed' WHERE connector = 'skipper';
UPDATE source_state SET source = 'session_feed' WHERE source = 'skipper';
