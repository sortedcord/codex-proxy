# Versioning and executable identity

The current version baseline is **v0.5.0** (the SemVer spelling of v0.5).
`internal/buildinfo/VERSION` is the source of truth for the baseline. Go module
language requirements and the upstream Codex client compatibility version are
independent of this application version.

## Release policy

Use SemVer `MAJOR.MINOR.PATCH`, with a `v` prefix on Git tags:

- During the v0 phase, major feature updates increment MINOR and reset PATCH:
  `v0.5.0` → `v0.6.0`.
- Fixes and small updates increment PATCH within the current feature series:
  `v0.5.0` → `v0.5.1`. Do not reset the minor number to zero for patch releases.
- v0 is a stabilization phase; compatibility is not yet promised. Document
  breaking changes explicitly in release notes, preferably in a feature release.
- Only the maintainer decides when the proxy setup is solid enough for `v1.0.0`.
  There is no automatic graduation to v1.
- From v1 onward: breaking compatibility increments MAJOR, backward-compatible
  features increment MINOR, and backward-compatible fixes increment PATCH.
- Prereleases may use suffixes such as `v0.6.0-rc.1`. Untagged working builds
  use `<baseline>-dev`; they must not masquerade as stable releases.

Update the baseline in the same reviewed change as a version bump. Create the
matching tag only after approval/merge. Adding version identification does not
itself publish a release or create a tag.

## Inspecting a build

```sh
codex-proxy version
# codex-proxy v0.5.0 (commit=<commit>, date=<RFC3339 timestamp>)
```

The output is one line on stdout; errors go to stderr. No flags or positional
arguments are accepted after `version`. It does not load configuration, contact
upstream, read credentials, acquire locks, or initialize history. Startup's
`listening` log includes the same version, commit, and date fields.

## Building

```sh
./scripts/build.sh                         # bin/codex-proxy; v0.5.0-dev
VERSION=v0.5.0 ./scripts/build.sh            # release identity, no publication
./bin/codex-proxy version
```

The script injects the Git commit and commit timestamp (not wall-clock build time,
so identical source builds are reproducible). A modified checkout's commit is
marked `-dirty`. `VERSION`, `COMMIT`, and `BUILD_DATE` may be supplied explicitly
by release tooling, including tooling building from a source archive without Git.
Metadata must contain no whitespace; `BUILD_DATE` should use RFC3339.

Plain `go build` uses the embedded baseline plus `-dev` and Go's VCS metadata
where available. `go install ...@v0.5.0` can use Go's main-module version as a
fallback. Missing commit/date metadata is reported as `unknown`, never empty.

Linker fields for future release tooling (#3):

```text
-X github.com/sortedcord/codex-proxy/internal/buildinfo.Version=v0.5.0
-X github.com/sortedcord/codex-proxy/internal/buildinfo.Commit=<commit>
-X github.com/sortedcord/codex-proxy/internal/buildinfo.Date=<RFC3339 timestamp>
```

Explicit linker metadata takes precedence over Go build information. Do not put
keys, tokens, or other runtime configuration in these fields.

Docker accepts equivalent `VERSION`, `COMMIT`, and `BUILD_DATE` build arguments.
Local Docker builds default to the embedded development baseline with unknown
commit/date because Git is intentionally excluded from the build context. The
publishing workflow injects the checked-out commit and commit timestamp, and uses
the tag for version builds or the baseline plus `-dev` for branch/PR builds.
Container tag channel changes remain separate work (#5).
