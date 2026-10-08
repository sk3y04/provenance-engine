# Repository and Module Rename

Phase 2 migrated the public repository identity from `provenance` to
`provenance-engine` while keeping the CLI binary and end-user command named
`provenance`.

## What changed

| Item | Before | After |
|------|--------|-------|
| GitHub repository | `sk3y04/provenance` | `sk3y04/provenance-engine` (manual rename) |
| Go module path | `github.com/sk3y04/provenance` | `github.com/sk3y04/provenance-engine` |
| Internal imports | `github.com/sk3y04/provenance/internal/...` | `github.com/sk3y04/provenance-engine/internal/...` |
| Binary / command | `provenance` | `provenance` (unchanged) |

The private product repository will separately take the name `provenance` and
depend on this module. Dependency direction is strictly:

```text
private provenance -> public provenance-engine
```

## Import-path break

Any external Go program that imported this module must update its import paths:

```go
// before
import "github.com/sk3y04/provenance/internal/..."
// after
import "github.com/sk3y04/provenance-engine/internal/..."
```

This is only a module-path rename. All implementation packages remain under
`internal/` and are not importable outside the module; the reusable public
facade arrives in Phase 4 (`engine/`).

## What did not change

- End-user command name, subcommands, flags, exit codes, and output formats.
- Persistence formats and paths (sessions, watches, history, collections,
  `_provenance_cache/`, `.provenance/` capture manifests).
- Cookies/credential handling and environment variables (for example
  `PROVENANCE_DATABASE_URL`, `CHROME_PATH`).
- yt-dlp progress protocol marker (`PROVENANCE_YTDLP_PROGRESS:`).
- License at the time of the rename (GPL-3.0). The project was subsequently
  relicensed under MIT; see `CHANGELOG`.

## Consumer steps

1. Update `go.mod` and all imports to the new module path.
2. `go mod tidy` (the old path no longer resolves; no `replace` directive is
   needed once the new tag is available).
3. No source-level API changes are required for existing CLI usage.

## Historical references

This document and `docs/MIGRATION_BASELINE.md` intentionally mention the old
module path `github.com/sk3y04/provenance` as historical context. The private
`provenance` module legitimately uses that path; this repository must never
import it.