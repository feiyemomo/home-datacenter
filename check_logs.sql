SELECT id, datetime(ts, 'unixepoch', 'localtime') as lt, event_type, level, message
FROM system_logs
ORDER BY ts DESC
LIMIT 30;
