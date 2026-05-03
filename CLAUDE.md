## Commands

```bash
go build ./...          
go test ./...           
go vet ./...
go fmt ./...            # required before every commit (see Workflow)
```

## Workflow

- **Version control is Jujutsu (`jj`), not git directly.** The repo is colocated (`.git` + `.jj`), so git tooling still reads it, but make changes with `jj` (`jj st`, `jj diff`, `jj new`, `jj describe`, `jj squash`). Don't run `git commit` / `git reset` / `git checkout` against this tree.
- Run `go fmt ./...` before describing/squashing a commit so diffs stay clean.
- Flag, don't silently follow, anything that goes against modern Go best practices
