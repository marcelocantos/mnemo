module github.com/marcelocantos/mnemo

go 1.26.1

require (
	github.com/fsnotify/fsnotify v1.9.0
	github.com/google/uuid v1.6.0
	github.com/marcelocantos/mcpbridge v0.0.0-20260406154701-a16a2c102bf2
	github.com/mark3labs/mcp-go v0.47.0
	github.com/mattn/go-sqlite3 v1.14.41
)

require golang.org/x/image v0.39.0 // indirect

require (
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/marcelocantos/sqldeep/go/sqldeep v0.19.0
	github.com/spf13/cast v1.7.1 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/sys v0.13.0 // indirect
)

replace github.com/marcelocantos/sqldeep/go/sqldeep => ../sqldeep/go/sqldeep
