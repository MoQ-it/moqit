# moqit - Claude Code Instructions

moqit depends on the moqt repository (MoQ transport library, Go + TS), pinned to a commit in `go.mod`. Locally it is checked out at `../moqt`; a git-ignored `go.work` makes Go use that checkout, and the web workspace always links `../moqt/ts`. Changes there follow the same rules below. After moqt changes are pushed, pin them with `GOWORK=off go get github.com/MoQ-it/moqt@<commit> && GOWORK=off go mod tidy` and run `GOWORK=off go test -race ./...`; the Docker image uses only the pinned version.

## After every code change

1. **Format** — Run `gofmt` on all changed Go files, in both repositories. For TypeScript there is no formatter; match the surrounding style and run `npm run typecheck` in `web/`.

2. **Regenerate protobuf code** — If anything in `proto/moqit/v1/*.proto` changed, run `go run github.com/bufbuild/buf/cmd/buf@v1.73.0 generate` from the repo root (it needs `npm install` in `web/` first, for `protoc-gen-es`). Never hand-edit the generated files — `proto/moqit/v1/*.pb.go`, `proto/moqit/v1/moqitv1connect/` and `web/packages/client/src/gen/` — manual edits are overwritten. Never reuse or renumber a proto field. Stage the regenerated code alongside the proto change.

3. **Keep the MoQT codec in step** — If the wire codec in `../moqt/wire/v22` changed, update the shared golden vectors in `../moqt/testdata/v22/` and the TS codec in `../moqt/ts` to match; both test suites read the same vectors.

4. **Update the docs** — `README.md` is the public landing page; reference detail lives in `docs/` (`meet.md`, `browser-sdk.md`, `server.md`, `deploy.md`, `clustering.md`, `protocol.md`, `development.md`). If SDK APIs (`@moqit/client`, `@moqit/react`, `sdk/go`), the RoomService API, tokens, webhooks, command-line flags (`cmd/moqit-server`, `cmd/meet`, `cmd/moqit`), the MoQ namespace/track layout or Meet features changed, update the matching `docs/` page, with a usage example for new API.
   - Keep `README.md` a landing page, under about **200 lines** (`wc -l README.md`). A new feature gets at most **one line** in its Features list — no flags, endpoints or caveats; those go in `docs/`.
   - Only claim what is tested; mark anything unverified (e.g. Safari, Docker) as such.

5. **Unit tests** — Every new feature, package, or extension to an existing package must have unit tests (Go: `go test -race ./...`; TS: vitest in `web/packages/*`).

6. **Integration and end-to-end tests** — Every new or updated feature must be covered at the level where it can break:
   - Server and cluster behaviour: `pkg/server` tests (real QUIC, miniredis).
   - SDK and media in real browsers: `test/e2e` (`go test -tags browser ./test/e2e`, Chrome and Firefox).
   - Meet UI: `web/apps/meet/e2e/meet.e2e.mjs` (`npm run build -w meet && npm run e2e -w meet`).
   Run the affected suites before finishing, and say which were run.

7. **Update the test commands** — If new test commands, suites or setup steps were added, update `docs/development.md`.

8. **Do not change API unless required** — Keep the public surface backwards compatible: proto messages and `RoomService`, the exported `@moqit/client` / `@moqit/react` / `sdk/go` APIs, the access token claims, webhook payloads, and MoQ namespace and track names. Prefer additive changes.

## Git commits

Never run `git commit` (or any other git operation that creates a commit, including `git commit --amend`, `git merge --no-ff`, `git rebase`, etc.). Stage changes and leave the commit to the user. This applies even when work is finished and "ready to commit" — stop at staging.

## Code comments

Keep code comments minimal. Default to no comment. Only add one when the **why** is non-obvious — a hidden constraint, a subtle invariant, a workaround for a specific bug, behaviour that would surprise a reader. One short line is almost always enough; never write multi-paragraph rationale, narrative, or war-story comments inline. Don't restate what well-named code already shows. Don't reference specific vendor implementations, past incidents, ticket numbers, or "we used to do X, now we do Y" in source — that belongs in commit messages or PR descriptions.
