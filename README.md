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

## Getting started

```sh
# See what the endpoint offers. This needs no API key, so it also proves
# the endpoint is reachable.
ypotitlo list-models

# Pick a model.
ypotitlo config-set model deepseek-v4-pro

# Provide a key, read from stdin so it stays out of your shell history.
# Skipped entirely if you already use opencode: its credentials are reused.
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

## Encodings

Input encoding is detected: BOMs, UTF-16 with or without one, and the legacy
Greek codepages (`windows-1253`, `ISO-8859-7`, `cp737`, `cp869`, MacGreek).
Pass `-charset` when a guess is wrong. Output is UTF-8 without a BOM unless you
ask otherwise; line endings and BOM follow the input when you do not.

Whatever is not translated comes back untouched — leading whitespace, inline
`<i>` and `<font>` markup, `{\an8}` positioning, bare ampersands. That is the
whole design constraint, and it is why this tool has its own SRT parser rather
than using an existing library.

## Cost

`translate` prints a summary of every run:

```
34 calls · 71204 in / 24880 out tokens · ~$0.2107 · 2 retries · 0 cues untranslated
```

`max_spend_usd` caps a run and defaults to $1. A model with no published price
cannot be costed, so the cap cannot be enforced for it; `list-models` says which
those are, and the summary says so too.

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

`base_url` is worth knowing about: the default pool carries only open-weight
models. Claude, GPT and Gemini live under `https://opencode.ai/zen/v1`.

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
