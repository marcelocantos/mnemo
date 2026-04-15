# Database connection failure

```
2026-04-15T10:23:41Z ERROR [db/pool.go:247] acquire connection:
  dial tcp 127.0.0.1:5432: connect: ECONNREFUSED
  commit abc12345 last known good
```

This fixture verifies that OCR preserves exact error strings
(`ECONNREFUSED`, port `5432`, commit hash `abc12345`, file path `db/pool.go:247`).
