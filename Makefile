.PHONY: bullseye build test vet fmt-check

BUILD_TAGS := sqlite_fts5

bullseye:
	@test -z "$$(gofmt -l .)" && echo "✓ fmt" || \
	 (echo "✗ gofmt issues:"; gofmt -l .; exit 1)
	@go vet -tags "$(BUILD_TAGS)" ./... && echo "✓ vet"
	@go build -tags "$(BUILD_TAGS)" -o bin/mnemo . && echo "✓ build"
	@go test -tags "$(BUILD_TAGS)" ./... 2>&1 | tail -20 && echo "✓ tests"
	@test -z "$$(git status --porcelain)" && echo "✓ clean" || \
	 (echo "✗ dirty tree:"; git status --short; exit 1)

build:
	go build -tags "$(BUILD_TAGS)" -o bin/mnemo .

test:
	go test -tags "$(BUILD_TAGS)" ./...

vet:
	go vet -tags "$(BUILD_TAGS)" ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
