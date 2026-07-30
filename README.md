# ypotitlo

Translate subtitle files into another language with an LLM.

`ypotitlo` reads an SRT file, translates the dialogue, and writes a new SRT with
every timing, cue boundary and piece of markup exactly where it was. It talks to
[OpenCode Zen](https://opencode.ai/docs/zen/), an OpenAI-compatible endpoint.

```
ypotitlo translate -i movie.en.srt -ol greek
# -> movie.el.srt
```

## Install

```
go install github.com/mtzanidakis/ypotitlo/cmd/ypotitlo@latest
```

Or download a binary from the [latest release](https://github.com/mtzanidakis/ypotitlo/releases/latest)
and put it on your `PATH`. Either way, later versions install themselves:

```
ypotitlo upgrade        # -n to see what it would do first
```

The archive's checksum is verified against the release's `checksums.txt` before
anything is replaced, and the swap is atomic — a failure at any point leaves the
working binary in place.

## Getting started

```sh
# See what the endpoint offers. This needs no API key, so it also proves
# the endpoint is reachable.
ypotitlo list-models

# Pick a model. deepseek-v4-flash translates a feature film for pennies;
# the reasoning models cost roughly ten times as much for the same file.
ypotitlo config-set model deepseek-v4-flash

# Provide a key, read from stdin so it stays out of your shell history.
# The same key works for both OpenCode Go and Zen, and is skipped entirely
# if you already use the opencode CLI: its credentials are reused.
ypotitlo config-set api_key -

# Check the plan before spending anything.
ypotitlo translate -i movie.en.srt -ol el -n

# Translate.
ypotitlo translate -i movie.en.srt -ol greek
```

## Commands

| Command | Purpose |
| --- | --- |
| `translate` | Translate a subtitle file |
| `list-models` | List the models the endpoint offers, with prices |
| `config-show` | Show the effective config and where each value came from |
| `config-set` | Set a configuration value |
| `config-unset` | Remove a configuration value |
| `upgrade` | Replace this binary with the newest release |
| `version` | Print the version |

Run `ypotitlo <command> -h` for a command's flags.

## Languages

A language may be given as a code or a name, in any case. `el`, `ell`, `gre`,
`greek`, `Greek` and `el-GR` all mean the same thing and all produce the same
output file, so re-running a command is never ambiguous.

The source language is optional. `ypotitlo` works it out from the filename, then
from the script, then from common words, and only asks the model as a last
resort — sampling from the middle of the file, because the first cues of a
subtitle are usually release credits rather than dialogue.

## Output filenames

Without `-o`, the output name replaces the input's language suffix:

| Input | Output (target `el`) |
| --- | --- |
| `movie.en.srt` | `movie.el.srt` |
| `movie.eng.srt` | `movie.el.srt` |
| `movie.srt` | `movie.el.srt` |
| `movie.eng.sdh.srt` | `movie.el.sdh.srt` |
| `movie.en.forced.srt` | `movie.el.forced.srt` |

Track markers such as `sdh`, `forced`, `cc` and `hi` are recognised and kept.
This matters more than it looks: `sdh` is also a valid language tag (Southern
Kurdish), as are `hi` (Hindi) and `cc` (Atsam), so treating them as languages
would delete the marker and quietly overwrite the ordinary translation. The
trade-off is that `.hi` is always read as *hearing impaired* rather than Hindi;
use `-o` for a genuine Hindi track.

`ypotitlo` refuses to write over its own input, and refuses to overwrite an
existing file without `-f`.

## Resuming

A run that fails partway still writes what it managed, leaving the rest in the
source language. `-resume` fills in only those:

```
ypotitlo translate -i movie.en.srt -resume
# resuming: 21 of 734 cues still to translate
```

It needs no state file. The output always has the same cues, in the same order,
with the same timings as its input, so a cue whose text still matches the source
is one that never got translated. The whole file is still read for the
consistency brief, so the cues filled in match the ones already there.

A failed run prints the exact command to finish it, so the next step is a paste
rather than a trip to the manual.

It also parks the consistency brief in a hidden file beside the output
(`.movie.el.srt.brief`) and picks it up on the resume, since the brief describes
the film rather than the attempt and costs about a minute to compute. That file
is written only when a run fails with work worth keeping, and removed as soon as
one finishes, so it exists exactly while it is useful. It is ignored if it was
written for a different subtitle or a different target language.

One imprecision that does not go away: a line that legitimately translates to
itself — a number, a name, `♪` — looks untranslated and is re-sent every time.
That costs a little and changes nothing, but a resume never reports zero
remaining.

## Encodings

Input encoding is detected: BOMs, UTF-16 with or without one, and the legacy
Greek codepages (`windows-1253`, `ISO-8859-7`, `cp737`, `cp869`, MacGreek).
Pass `-charset` when a guess is wrong. Output is UTF-8 without a BOM unless you
ask otherwise; line endings and BOM follow the input when you do not.

Whatever is not translated comes back untouched — leading whitespace, inline
`<i>` and `<font>` markup, `{\an8}` positioning, bare ampersands. That is the
whole design constraint, and it is why this tool has its own SRT parser rather
than using an existing library.

## OpenCode Go and OpenCode Zen

These are two products behind one account, and **the same API key works for
both**. Which one you use is decided entirely by `base_url`:

| | `base_url` | Billing | Models |
| --- | --- | --- | --- |
| **OpenCode Go** (default) | `https://opencode.ai/zen/go/v1` | Monthly subscription | 23, open-weight |
| **OpenCode Zen** | `https://opencode.ai/zen/v1` | Pay as you go | 60+, adds Claude, GPT and Gemini |

`ypotitlo` defaults to Go, since a subscription is the sensible way to pay for
something that runs in batches. Switch per run or for good:

```sh
ypotitlo translate -i movie.en.srt -ol el -base-url https://opencode.ai/zen/v1 -m claude-sonnet-5
ypotitlo config-set base_url https://opencode.ai/zen/v1
```

If you already use the `opencode` CLI, its key is picked up automatically and
there is nothing to configure.

## Cost

`translate` prints a summary of every run:

```
34 calls · 73778 in / 388006 out tokens · ~$0.1190 · 0 retries · 0 cues untranslated
```

**On a Go subscription that dollar figure is not your bill.** It is computed
from the published pay-as-you-go list prices, because that is the only pricing
the API exposes; the subscription's marginal cost is zero until you hit its
limits. Read it as a measure of how much work a run did — useful for comparing
models and batch sizes — rather than as money leaving your account. On Zen it is
a genuine estimate.

`max_spend_usd` caps a run and defaults to $1. It stops the run *before* the
request that would cross it. Note the same caveat: on a subscription it is a
usage ceiling rather than a spending one.

Two things it cannot do. A model with no published price cannot be costed at
all, so the cap never engages for it — `list-models` marks those, and the run
summary says so. And thinking is charged as output: on a reasoning model the
same subtitle file can cost ten times what it does on a plain one, which is why
the summary reports output tokens separately.

The other ceilings still apply to unpriced models: a call fuse of
`3 × batches + 10`, a two-hour `-timeout`, and a watchdog that gives up when
nothing has advanced for six minutes. The timeout is sized for a long film —
a 2h40m one is about a hundred batches and takes roughly fifty minutes — since
exceeding it writes nothing, discarding work already paid for.

## Configuration

`~/.config/ypotitlo/config.toml`, or `$XDG_CONFIG_HOME/ypotitlo/config.toml`.

Every setting resolves as **flag → environment → config file → default**, and
`config-show` prints which one won:

```
$ ypotitlo config-show
config file: /home/you/.config/ypotitlo/config.toml

SETTING          VALUE                          SOURCE
base_url         https://opencode.ai/zen/go/v1  config
model            deepseek-v4-pro                config
api_key          ...lanc (64 chars)             env OPENCODE_API_KEY
target_language  el                             config
...

note: api_key is also set in config, shadowed by env OPENCODE_API_KEY
```

The API key is looked for in `-api-key`, then `$OPENCODE_API_KEY`, then
`$OPENCODE_ZEN_API_KEY`, then the config file, and finally in opencode's own
`auth.json` — so an existing opencode install needs no configuration at all.
One key serves both endpoints; see [OpenCode Go and OpenCode
Zen](#opencode-go-and-opencode-zen) for what `base_url` selects.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Runtime error |
| 2 | Usage error |
| 3 | The input could not be parsed |
| 4 | The API key was rejected, or the account is out of credit |
| 5 | The spend ceiling was reached |
| 130 | Interrupted; nothing was written |

Output is written atomically, so an interrupted or failed run leaves the
previous file intact rather than a half-translated one.

## Development

```
mise run build
mise run test
mise run lint
```

See [AGENTS.md](AGENTS.md) for the project's conventions.

## Licence

MIT. See [LICENSE](LICENSE).
