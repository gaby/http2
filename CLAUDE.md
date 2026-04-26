# Context routing

This repo uses context-mode for context preservation. Default to context-mode
tools over native equivalents whenever a command, file read, or fetch could
return more than ~20 lines.

## Use context-mode

- File analysis (counting, parsing, extracting): `ctx_execute_file`
- Running tests, builds, CLIs: `ctx_execute`
- Fetching external docs/HTML: `ctx_fetch_and_index` then `ctx_search`
- Multiple files at once: `ctx_batch_execute`

## Use native tools

- Files you need to edit (read-then-edit cycle): native `view` + `edit`
- Small file mutations: native `bash` for `mkdir`, `mv`, `rm`, `chmod`,
  `git add`/`commit`/`push`, `cd`, `echo`
- Anything guaranteed under ~20 lines

When in doubt, use context-mode. Every KB of unnecessary output reduces
context-window quality for the rest of the session.

# Project instructions

- Always run the full test suite with the race detector: `go test -race ./...`.
- Always run modernize with: `go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -fix -test=false ./...
`
- Report test results in the final summary, including the `-race` invocation.
- Once tests pass once, run the tests at least 10-20 times to detect timing issues.
