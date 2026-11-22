# Porting the service to Go

The Python package `infomaster/` is being rewritten in Go in this `go/` tree (D81). Python stays the running service
until the Go port matches it; then Go takes over the same `data/` directory, SQLite database and clients in one
release. Until then both trees live in `main`, and CI checks both.

## Rules for every package

- **The Python code is the spec.** Port behaviour, names of stored things, SQL, JSON shapes, texts shown to the owner
  (both languages) and comments that explain why. Port the module's pytest cases as Go tests in the same package
  (`*_test.go`); they are the acceptance test of the port.
- **External contract byte for byte**: see `CONTRACT.md` (schema, kv keys, settings, routes, JSON shapes, uid and
  hash formulas, stored credential formats). Anything stored or compared as text must look exactly as Python writes
  it.
- **Foundation packages** (use them, do not duplicate them):
  - `internal/store`: `store.Open(path)`, `s.DB` (`*sql.DB`, one connection), `GetJSON/PutJSON/DeleteJSON` for `kv`.
  - `internal/clock`: `Now` (replaceable in tests), `NowUTC`, `LocalDay`, `DayStartUTC`, `UTCISO`, `LocalISO`,
    `ParseLocal`, `ParseUTC`, layouts. Moscow local time without a zone everywhere except `llm_runs.at` (UTC).
  - `internal/pyjson`: `Dumps` (= `json.dumps(ensure_ascii=False)`, with `", "` and `": "`), `DumpsASCII`
    (= plain `json.dumps`), `Compact`, `CompactASCII`. Use it for every JSON written to the database or hashed.
    Python writes floats as `8.0`: use `pyjson.Float` for float fields of stored or served JSON.
  - `internal/i18n`: `Phrase` (`En`, `Ru`, `Alert`), `P`, `AlertP`, `Tr`, `Err`, `Reason`, `Localize`.
  - `internal/pytext`: `Casefold`, `Split`, `Strip`, `IsSpace`, `Len`, `Cut` (code points) and `Quote(s, safe)`:
    Python's `str.casefold`, `str.split`, `len`, slicing and `urllib.parse.quote`.
  - `internal/tgid`: Telethon's marked chat ids.
  - `internal/config`: `Env` from the environment.
- Traps found while writing `CONTRACT.md` (its "porting traps" section has the full list): Python's `casefold()` and
  `str.split()` differ from `strings.ToLower` and `strings.Fields`; lengths and slices count code points (runes);
  HTTP answers except MCP are ASCII-escaped JSON with Python separators and floats like `8.0`; `quote()` matches
  neither Go escape function.
- Python callbacks set after construction (`obj.notify = ...`) become exported function fields on the Go struct.
- asyncio tasks become goroutines with a `context.Context`; `run()` loops take a context and stop when it is done.
- **Do not edit `go.mod` or `go.sum`, and do not run `go mod tidy` or `go get`.** If a library is missing, say so in
  the report instead; the main session adds it.
- Own only the packages your brief names; do not edit other packages. If a foundation package needs a change, say so
  in the report.
- English only. No personal data (real names, group numbers, chat titles, hosts, IPs, `/Users` paths) in code, tests
  or comments; tests use made-up data. No mention of AI tools as authors anywhere.
- Before reporting: `gofmt -l .` prints nothing, `go vet ./...` and `go test ./...` pass (from `go/`).
- Report briefly: what was ported, which Python tests were ported and which were not (and why), any behaviour that
  could not be matched, and changes the foundation needs.
