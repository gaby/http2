# Repository agent instructions

- Always run the full test suite with the race detector: `go test -race ./...`.
- Always run modernize with: `go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -fix -test=false ./...
`
- Report test results in the final summary, including the `-race` invocation.
- Once tests pass once, run the tests at least 10-20 times to detect timing issues.
