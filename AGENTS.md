# AGENTS.md

Conventions for working on `ypotitlo`.

## Commands

| Task | Command |
| --- | --- |
| Build | `mise run build` |
| Test | `mise run test` |
| Test with no home directory | `mise run test-nohome` |
| Lint | `mise run lint` |
| Format | `mise run fmt` |
| Tidy modules | `mise run tidy` |
| Verify modules | `mise run verify` |
| Lint the last commit | `mise run commitlint` |

Go 1.26.5 and golangci-lint 2.12.2, both pinned in `mise.toml`.

## The golden rule

**Whatever is not translated must come back unchanged.**

Every design decision in this repo follows from that. A subtitle round trip
passes through a parser, an encoder, a network protocol and a language model,
and each of those is an opportunity to silently alter text nobody asked to
change. Leading whitespace, inline `<i>` markup, `{\an8}` positioning, a bare
ampersand, cue ordering, cue count, timings — all of it is data the user did not
ask us to touch.

This is why the project has its own SRT parser rather than importing one. The
obvious library joins words across adjacent tags, escapes ampersands
asymmetrically and trims leading whitespace. None of those failures produces an
error; they produce a slightly wrong file.

Concretely:

- A cue is `{Index, Start, End, Lines}` where `Lines` is **opaque**. The parser
  never interprets markup. Markup is touched only at the model boundary and put
  back afterwards.
- Never `TrimSpace` cue text.
- Never sort, deduplicate or drop cues, even malformed ones.
- Never escape on write.
- A translated file has exactly as many cues as its input, in the same order,
  with the same timings. A cue that could not be translated keeps its original
  text and is counted.

## Silence is the enemy

A wrong file that reports success is worse than a crash. When something is
guessed, say so; when something is dropped, say so; when a value came from
somewhere surprising, say where.

- Recoverable problems go through a `Warn` seam, never `os.Stderr` directly, so
  tests can assert on them.
- Guesses are reported with their provenance: "Greek (from the filename)" tells
  a user with a mislabelled file exactly which assumption is wrong.
- A cost that cannot be computed is reported as *unknown*, never as zero. A zero
  makes the spend ceiling silently inert.
- `config-show` prints where each value came from, because the question users
  actually arrive with is "why did it use that?".

## Code style

- Standard library first. Two dependencies, both pure Go: `golang.org/x/text`
  and `github.com/pelletier/go-toml/v2`. No web framework, no CLI framework,
  no assertion library.
- **The stdlib `flag` package is not negotiable.** It looks names up by exact
  match, so `-il` is one flag. cobra/pflag apply POSIX shorthand clustering and
  read `-il` as `-i l` — silently, with no error. A test pins this.
- Injectable seams for anything that touches the world: `Now`, `Sleep`,
  `Getenv`, `GOOS`, `*http.Client`, `*rand.Rand`, `Warn`, `Progress`. The whole
  CLI runs through `run(ctx, env) int` so `main` holds no logic and every
  subcommand is table-testable against a buffer.
- Comments explain *why*, not *what*. A comment that restates the code is noise;
  a comment recording the measurement that made a threshold what it is, or the
  failure a guard prevents, is the most valuable thing in the file.

## Tests

- Standard `testing` only. Table-driven, `t.Parallel()` everywhere.
- No test touches the network, the real home directory or anything outside
  `t.TempDir()`. `mise run test-nohome` runs the suite with `HOME=/nonexistent`
  and CI fails if a package reaches for the real thing.
- `_test.go` files **are** linted, unlike the repo this was modelled on. The
  fixture round-trips carry most of the correctness burden here.
- Fixtures live in `testdata/`. `.gitignore` excludes `*.srt` so scratch files
  never get committed, with an explicit `!**/testdata/**` so fixtures do.
- Prefer a test that pins a decision over one that pins an implementation. Where
  behaviour is a deliberate trade-off — a leading space meaning a line is not an
  index, `.hi` meaning hearing-impaired — there is a test saying so, so that the
  next person changes it on purpose rather than by accident.

## Deliberate divergences

Where this tool knowingly differs from a reference implementation, the reason
belongs at the divergence site as a comment. The current set:

- **ffmpeg's SRT reader** drops a cue's trailing numeric text line, discards
  empty cues and re-sorts by timestamp. We keep all three, because cue count and
  order are part of the contract.
- **ffmpeg's millisecond parsing** reads `,5` as 5ms. We read it as 500ms: the
  separator is a decimal point, and `,5` in the wild means half a second.
- **`golang.org/x/net/html/charset`** is not used. It hunts for `<meta>` tags
  and falls back to windows-1252, which mojibakes every legacy Greek file.
- **ICU-derived charset detectors** decide windows-1253 against ISO-8859-7 on a
  single "any C1 byte" boolean. We use an ordered ladder; see `charset/detect.go`
  for what was measured.

## Commits

Conventional Commits, enforced by commitlint in CI. Types and scopes come from
`.commitlintrc.yml`; scopes mirror the `internal/` package names plus `cli`,
`ci`, `docs`, `deps` and `build`. Header ≤ 100 characters, body lines ≤ 100, no
trailing full stop in the subject. Subject case is unrestricted because subjects
routinely start with SRT, BOM, UTF-8 or LLM.

Write the body for someone reading `git log` in a year with no memory of the
conversation. Say what changed and why it had to change; if a decision was
counter-intuitive, say what would have gone wrong otherwise.

Each phase of work ends with a green `mise run lint && mise run test` and one
commit. Verify a message with `mise run commitlint`.

## Versions

`main.version` is stamped by the linker in release builds, but `go install` passes
no ldflags, so a binary installed that way would report `dev` despite coming from
a tagged module. `resolveVersion` therefore prefers the stamped value and falls
back to the module version in `runtime/debug.ReadBuildInfo`, normalising both to a
leading `v`. Keep those two sources in agreement if either changes: `upgrade`
compares the reported version against the latest release tag, so a version that
lies makes it either refuse a real upgrade or attempt a pointless one.

## Releasing

Push a `v*` tag. `release.yml` gates GoReleaser behind lint and tests, and
GoReleaser publishes a **draft** release so the notes can be reviewed before
they go out. Both `mise.toml` and `.goreleaser.yaml` stamp `main.version`, so a
local build and a released binary report the version the same way — keep it that
way if either is changed.
