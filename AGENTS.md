# Context routing

This repo uses context-mode for context preservation. Default to context-mode tools
whenever a command, file read, or fetch could return more than ~20 lines.

## Bash whitelist (run directly)

- File mutations: `mkdir`, `mv`, `cp`, `rm`, `touch`, `chmod`
- Git writes: `git add`, `git commit`, `git push`, `git checkout`, `git branch`, `git merge`
- Navigation / process: `cd`, `pwd`, `which`, `kill`
- Package install: `go get`, `go mod tidy`, `npm install`, `pip install`
- Trivial output: `echo`, `printf`

Everything else goes through context-mode — `gh`, `kubectl`, `docker`, builds,
test runs, log inspection, any file read larger than a few lines.

## Tool selection

| Situation | Tool | Example |
|---|---|---|
| Run tests | `ctx_execute` | `go test -race ./...`, `go test -run TestServerConn` |
| Run lint / build | `ctx_execute` | `golangci-lint run`, `go vet ./...`, `make test` |
| Run h2spec / fuzz | `ctx_execute` | `h2spec -p 8080`, `go test -fuzz=Fuzz` |
| Git history / diffs | `ctx_execute` | `git log --oneline -50`, `git diff HEAD~5` |
| GitHub CLI | `ctx_execute` | `gh pr list --json ...`, `gh run view ...` |
| Read a Go source file | `ctx_execute_file` | Find TODOs, count handlers, extract signatures |
| Read a log / coverage file | `ctx_execute_file` | Parse `coverage.out`, parse build logs |
| Fetch external docs | `ctx_fetch_and_index` → `ctx_search` | RFC 7540, fasthttp docs |
| Multiple commands at once | `ctx_batch_execute` | Run tests + lint + build in parallel |

## Decision tree

```
About to run a command, read a file, or call an API?
│
├── Whitelisted (mkdir/mv/git write/echo/install)?
│   └── Bash
│
├── Output might exceed ~20 lines, or unsure?
│   └── ctx_execute (commands) / ctx_execute_file (files)
│
├── Fetching external docs or HTML?
│   └── ctx_fetch_and_index → ctx_search
│
└── Reading a file you need to EDIT?
    └── Native view + edit (context-mode is for analysis, not editing)
```

## Critical rules

1. Always print findings — only stdout enters context. No output = wasted call.
2. Analyze inside the sandbox, don't dump raw data. Print summaries with line numbers, IDs, exact values.
3. For files you need to edit: native view + edit. context-mode is for analysis only.
4. Never `ctx_index(content: large_data)` — use `ctx_index(path: ...)` so reads happen server-side.
5. Don't re-index data already in context — use it directly, or save to file and re-read.
6. When uncertain, default to context-mode. Every KB of unnecessary output costs the whole session.

## Anti-patterns

- `cat coverage.out` → entire file floods context. Use `ctx_execute_file`.
- `go test ./... -v` → full test output in context. Use `ctx_execute` to capture and summarize.
- `gh pr list` → raw JSON. Use `ctx_execute` with a `--jq` filter.
- Piping through `| head -20` — the rest is lost. Use `ctx_execute` to analyze the full output and print a summary.
- `curl https://...` — response body floods context. For external docs or HTML, use `ctx_fetch_and_index` → `ctx_search`; for API calls you need to analyze in the sandbox, use `ctx_execute` and print a summary.

# Project instructions

- Always run the full test suite with the race detector: `go test -race ./...`.
- Always run modernize: `go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -fix -test=false ./...`
- Report test results in the final summary, including the `-race` invocation.
- Once tests pass once, run the tests at least 10-20 times to detect timing issues.
