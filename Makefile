## help: 💡 Display available commands
.PHONY: help
help:
	@echo '⚡️ Development:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' |  sed -e 's/^/ /'

## fmt: 🎨 Fix code format issues
.PHONY: fmt
fmt:
	goimports -w .
	gofmt -w -s .

## audit: 🚀 Conduct quality checks
.PHONY: audit
audit:
	go mod verify
	go vet ./...
	GOTOOLCHAIN=$(GOVERSION) go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## benchmark: 📈 Benchmark code performance
.PHONY: benchmark
benchmark:
	go test ./... -benchmem -bench=. -run=^Benchmark_$

## coverage: ☂️  Generate coverage report
.PHONY: coverage
coverage:
	GOTOOLCHAIN=$(GOVERSION) go run gotest.tools/gotestsum@latest -f testname -- ./... -race -count=1 -coverprofile=/tmp/coverage.out -covermode=atomic
	go tool cover -html=/tmp/coverage.out

## format: 🎨 Fix code format issues
.PHONY: format
format:
	GOTOOLCHAIN=$(GOVERSION) go run mvdan.cc/gofumpt@latest -w -l .

## test: 🚦 Execute all tests
.PHONY: test
test:
	go run gotest.tools/gotestsum@latest -f testname -- ./... -race -count=1 -shuffle=on

## modernize: 🛠 Run gopls modernize
.PHONY: modernize
modernize:
	GOTOOLCHAIN=$(GOVERSION) go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -fix -test=false ./...

## longtest: 🚦 Execute all tests 10x
.PHONY: longtest
longtest:
	GOTOOLCHAIN=$(GOVERSION) go run gotest.tools/gotestsum@latest -f testname -- ./... -race -count=100 -shuffle=on

## betteralign: 📐 Optimize alignment of fields in structs
.PHONY: betteralign
betteralign:
	GOTOOLCHAIN=$(GOVERSION) go run github.com/dkorunic/betteralign/cmd/betteralign@v0.8.0 -test_files -generated_files -apply ./...

## generate: ⚡️ Generate implementations
.PHONY: generate
generate:
	go generate ./...

# actionspin: 🤖 Bulk replace GitHub actions references from version tags to commit hashes
.PHONY: actionspin
actionspin:
	GOTOOLCHAIN=$(GOVERSION) go run github.com/mashiike/actionspin/cmd/actionspin@latest
