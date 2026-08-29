module github.com/marcelocantos/mnemo

go 1.26.1

require (
	github.com/fsnotify/fsnotify v1.9.0
	github.com/google/uuid v1.6.0 // indirect
	github.com/marcelocantos/claudia v0.22.0
	github.com/mark3labs/mcp-go v0.47.0
	github.com/mattn/go-sqlite3 v1.14.41
	golang.org/x/image v0.39.0
)

require gopkg.in/yaml.v3 v3.0.1

require github.com/marcelocantos/sqlift/go/sqlift v0.17.0

require (
	github.com/dop251/goja v0.0.0-20260701091749-b07b74453ea9
	github.com/fsnotify/fsevents v0.2.0
	github.com/klauspost/compress v1.19.2
	github.com/yuin/goldmark v1.8.2
)

require (
	github.com/aws/aws-sdk-go-v2 v1.43.3 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.16 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.34 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.33 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.34 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.34 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.34 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.35 // indirect
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.57.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.15 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.34 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.5.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.33.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.38.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.45.3 // indirect
	github.com/aws/smithy-go v1.27.6 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/dlclark/regexp2/v2 v2.2.1 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	golang.org/x/text v0.36.0 // indirect
)

require (
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/marcelocantos/sqldeep/go/sqldeep v0.23.0
	github.com/spf13/cast v1.7.1 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/sys v0.43.0
)

replace github.com/marcelocantos/sqldeep/go/sqldeep => ../sqldeep/go/sqldeep
