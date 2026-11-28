# stt-router

A self-hosted speech-to-text account router. It is like CLIProxyAPI, but for speech-to-text.

You run one small binary on your machine. Your apps send OpenAI-style transcription requests to it. The router sends each request to one of your provider accounts (Groq, OpenAI, ElevenLabs, Microsoft MAI-Transcribe-2 on Azure Speech, or any OpenAI-compatible server). When an account hits a rate limit, a quota, or an error, the router puts that account in a cooldown and tries the next account or the next provider.

## Features

- One static Go binary, standard library only. Linux first; it also builds for macOS and Windows.
- OpenAI-compatible `POST /v1/audio/transcriptions`, so OpenAI SDKs and tools work without changes.
- A Realtime transcription WebSocket at `GET /v1/realtime` for live audio, with a simple energy VAD. Each turn is one normal batch request.
- All five response formats (`json`, `text`, `verbose_json`, `srt`, `vtt`) for every provider. The router makes the subtitles itself, so `srt` and `vtt` also work with Groq and ElevenLabs.
- ElevenLabs Scribe mapped to the OpenAI shapes, with word timestamps, speakers, and segments.
- Microsoft MAI-Transcribe-2 through the Azure Speech fast transcription API, mapped the same way.
- Model names with fallback chains, for example `auto` = Groq first, then ElevenLabs.
- Least recently used choice among accounts, with weights, concurrency limits, and per-account file size limits.
- Cooldowns from `Retry-After`, from "try again in 1m2.5s" text, or exponential backoff. Bad keys are marked invalid.
- Cooldowns, counters, and the request log survive a restart.
- The upload is read once and replayed for each attempt. It is never decoded or re-encoded, unless an `azure-speech` group sets `convert: "ffmpeg"`.
- A built-in dashboard: totals, a routing view, account states with live countdowns, models, client keys, and the request log.

## Quick start

### Build

With Docker (no Go needed on the host):

```sh
make docker-build        # writes ./stt-router
./stt-router -version
```

With Go 1.24 or later:

```sh
make build
```

Other targets: `make test`, `make bench`, `make docker-test` (vet, gofmt, tests, and the race detector in Docker).

To build for another OS, set `GOOS` and `GOARCH`, for example `GOOS=windows GOARCH=amd64 make build`.

### First run

```sh
./stt-router
```

At the first start the router:

1. Creates `~/.config/stt-router/config.json` (mode 0600, directory 0700) with a Groq group, an ElevenLabs group, no accounts, and three models.
2. Generates a management key, saves it in the config, and prints it once on stdout.
3. Listens on `http://127.0.0.1:8330`.

Open `http://127.0.0.1:8330/` and sign in with the management key.

### Add accounts

In the dashboard, click "Add account" in a provider group. Or edit the config file and restart. See `config.example.json` for a full example.

You can keep keys out of the config file. A key value `env:NAME` reads the key from the environment variable `NAME`.

### Send a request

```sh
curl http://127.0.0.1:8330/v1/audio/transcriptions \
  -F file=@meeting.mp3 \
  -F model=auto \
  -F response_format=srt
```

If you set client keys, add `-H "Authorization: Bearer <client key>"`.

### Use an OpenAI SDK

Point the SDK base URL at the router. The SDK needs some API key; use a client key, or any text when no client keys are set.

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8330/v1", api_key="sk-your-client-key")
with open("meeting.mp3", "rb") as f:
    srt = client.audio.transcriptions.create(model="auto", file=f, response_format="srt")
print(srt)
```

```js
import OpenAI from "openai";
import fs from "node:fs";

const client = new OpenAI({ baseURL: "http://127.0.0.1:8330/v1", apiKey: "sk-your-client-key" });
const r = await client.audio.transcriptions.create({ model: "auto", file: fs.createReadStream("meeting.mp3") });
console.log(r.text);
```

### Run as a service

`deploy/stt-router.service` is a systemd user unit. The install steps are in the file.

## Commands

```sh
stt-router              # run the router
stt-router dashboard    # open the dashboard and copy the management key to the clipboard
stt-router status       # also: start, stop, restart (systemd user service, Linux)
stt-router logs         # follow the service log (Linux)
stt-router version
stt-router help
```

`dashboard` needs `wl-copy`, `xclip` or `xsel` on Linux for the clipboard. It never prints the key. Options such as `-config` go before the command.

## Client API

| Method and path | Description |
| --- | --- |
| `POST /v1/audio/transcriptions` | Transcribe an upload (multipart form). |
| `GET /v1/realtime` | Live transcription over a WebSocket (see below). |
| `GET /v1/models` | The model names this router serves. |
| `GET /healthz` | Always `200` with `{"status":"ok"}`. No auth. |

Form fields of `/v1/audio/transcriptions`:

| Field | Notes |
| --- | --- |
| `file` | Required. Any audio or video format the upstream accepts. |
| `model` | Required. A model name from the config `models`. |
| `language` | Optional. Sent as `language` (OpenAI type), `language_code` (ElevenLabs), or `locales` (Azure Speech). |
| `prompt` | Optional. OpenAI type only. |
| `temperature` | Optional number. |
| `response_format` | `json` (default), `text`, `verbose_json`, `srt`, `vtt`. |
| `timestamp_granularities[]` | `segment`, `word`. With `word`, the router asks the upstream for word timestamps. |
| other fields | Passed to the upstream when its type knows them. ElevenLabs: `diarize`, `num_speakers`, `diarization_threshold`, `tag_audio_events`, `keyterms`, `seed`, `file_format`. Azure Speech: `diarize`, `num_speakers`, `keyterms`, `transcribe_style` (see below). Repeated values are kept. Other fields are dropped. |

Errors use the OpenAI error shape:

```json
{"error": {"message": "...", "type": "invalid_request_error", "param": null, "code": "..."}}
```

| Status | When |
| --- | --- |
| 400 | Bad form: no file, no model, bad `response_format`, or bad `temperature`. Also a format that every account refuses (`plain_json`), a bad `num_speakers` for Azure Speech, and audio that `ffmpeg` could not convert when no other target is left (`invalid_audio`). |
| 401 | Missing or wrong client key. |
| 404 | Unknown model. |
| 413 | Upload over `max_upload_mb`, or over the `max_file_mb` of every account. |
| 415 | `unsupported_media_type`: no target can take the audio format. Only `azure-speech` targets refuse formats (they take WAV, MP3 and FLAC). |
| 429 | All accounts are cooling down or busy. `Retry-After` is the soonest cooldown end, in seconds. |
| 503 | No enabled account for the model. |
| other 4xx | The upstream refused the request (bad audio, bad parameter). The upstream error is passed through. |

## Realtime (WebSocket)

The realtime endpoint gives live transcription for apps such as OpenWhispr. Each turn of speech becomes one normal batch request, so the model chains, accounts, cooldowns, request log, and totals are the same as for `/v1/audio/transcriptions`.

URL: `ws://127.0.0.1:8330/v1/realtime?intent=transcription`. Send the client key as `Authorization: Bearer <key>`, the same rule as the batch API. A request without an Upgrade header answers `426`. A query parameter `model=<name>` sets the router model; without it, the session's `transcription.model` is used when the router serves that name, else `auto`.

Events from the client:

| Event | Meaning |
| --- | --- |
| `session.update` | Optional. Sets the rate (8000 to 48000, default 24000), `transcription.model` and `transcription.language`, and `turn_detection` (`server_vad` with `threshold`, `silence_duration_ms`, `prefix_padding_ms`, or `null` for no VAD). |
| `input_audio_buffer.append` | Base64 of 16-bit little-endian mono PCM at the session rate. |
| `input_audio_buffer.commit` | Ends the current turn at once. |

Events from the server: `session.created`, `session.updated`, `input_audio_buffer.speech_started`, `input_audio_buffer.speech_stopped`, `input_audio_buffer.committed`, `conversation.item.input_audio_transcription.completed` (`item_id`, `transcript`), `conversation.item.input_audio_transcription.failed` (`item_id`, `error`), and `error`. A commit with no speech answers an `error` with code `input_audio_buffer_commit_empty`. There are no `delta` events.

VAD rule: frames of 20 ms, by RMS energy. A frame is speech when its RMS is above `max(-45 dBFS, 3x the noise floor)`. The noise floor adapts slowly on frames that are not speech. A turn starts after 100 ms of speech frames. It ends after `silence_duration_ms` of non-speech (default 600). It keeps `prefix_padding_ms` of audio before the start (default 500). A turn is cut at 15 s (60 s with `turn_detection: null`) and the next turn continues without lost audio. A turn with less than 300 ms of speech is dropped with no upstream request.

Each turn is wrapped in a WAV header in memory and sent through the batch routing with `response_format=json`, plus `language` when set. One goroutine transcribes the turns in order. At most 8 turns wait; a full queue drops the oldest pending turn with a `failed` event. A failed turn answers `conversation.item.input_audio_transcription.failed`, not `error`.

## Response formats per provider

| `response_format` | `openai` type | `openai` with `plain_json` | `elevenlabs` type | `azure-speech` type |
| --- | --- | --- | --- | --- |
| `json` | yes | yes | yes | yes |
| `text` | yes | yes | yes | yes |
| `verbose_json` | yes, from the upstream `verbose_json` | refused | yes, built from the words | yes, one segment per phrase, with words |
| `srt`, `vtt` | yes, from the upstream segments | refused | yes, from segments built from the words | yes, one cue per phrase |

For the `openai` type, the router always asks the upstream for `verbose_json` with segment timestamps, and converts it. This gives the audio duration for the usage counters. Set `plain_json: true` on an account whose upstream supports only `json` and `text`. For such an account, a request that needs timestamps skips the account and goes to the next one.

For ElevenLabs, `verbose_json` has `words` with `speaker` (from `speaker_id`) when diarization is on. Segments start a new line at a sentence end, at a silence over 1 second, and at a speaker change. Segments have a `speaker` field too.

## Azure Speech (MAI-Transcribe-2)

The `azure-speech` type sends the upload to the Azure Speech fast transcription API with Microsoft's MAI-Transcribe models. You need a Speech (Foundry) resource in a region that offers MAI-Transcribe. Only some Speech regions offer it; see the "Transcribe with MAI-Transcribe" column in the Microsoft Speech service regions list.

```json
"providers": {
  "azure": {
    "type": "azure-speech",
    "base_url": "https://your-resource.cognitiveservices.azure.com",
    "convert": "ffmpeg",
    "accounts": [{ "name": "azure-1", "api_key": "env:AZURE_SPEECH_KEY" }]
  }
},
"models": {
  "mai-transcribe-2": [{ "provider": "azure", "model": "MAI-Transcribe-2" }]
}
```

`base_url` is the resource endpoint. `api_key` is a resource key. The target `model` is the Azure model name, for example `MAI-Transcribe-2`.

The request is `POST {base_url}/speechtotext/transcriptions:transcribe?api-version=2025-10-15` with the header `Ocp-Apim-Subscription-Key`. The form has an `audio` part (the file) and a `definition` part (JSON):

| Client field | `definition` |
| --- | --- |
| target `model` | `enhancedMode: {"enabled": true, "model": "..."}` |
| `timestamp_granularities[]=word`, or `verbose_json`, `srt`, `vtt` | `enhancedMode.modelOptions.timestamps: "word"`. Else `"segment"`, which gives the duration for the usage counters. |
| `transcribe_style` | `enhancedMode.modelOptions.transcribeStyle` (`verbatim`, the Azure default, or `clean`). |
| `language` | `locales: ["<language>"]`. Without it, Azure detects the language. |
| `diarize=true` | `diarization: {"enabled": true}` |
| `num_speakers` | `diarization.maxSpeakers`, only with `diarize=true`. Must be a positive integer. |
| `keyterms` (repeated) | `phraseList: {"phrases": [...]}` |
| always | `profanityFilterMode: "None"`, so that the transcript keeps the words as spoken. Azure masks profanity by default. |

`prompt` and `temperature` have no Azure field; the router drops them.

The response maps as follows: `text` is the `combinedPhrases` texts joined with a space. `language` is the locale of the first phrase. `duration` is `durationMilliseconds` (or the end of the last phrase). Each phrase becomes one segment, with `speaker` set to `speaker_<n>` when diarization is on. `words` come from the phrase words, with the phrase speaker.

### Audio formats and `convert`

MAI-Transcribe takes WAV, MP3, and FLAC only. The router finds the format from the first bytes of the upload, not from the file name, and sends the matching file name and content type.

| Group `convert` | WAV, MP3, FLAC | Other formats (Ogg Opus, WebM, M4A, ...) |
| --- | --- | --- |
| absent or `"off"` | sent as they are | the target is skipped with no penalty for its accounts, and the next target gets the upload. With no other target, the answer is 415 `unsupported_media_type`. |
| `"ffmpeg"` | sent as they are; converted when `diarize=true` | converted to 16 kHz mono FLAC |

With `convert: "ffmpeg"`, the router runs `ffmpeg` once per request, at most 2 minutes, with temp files in the spool directory that it always removes. A client abort stops `ffmpeg`. All accounts of the group get the converted file; other targets get the original bytes. If the conversion fails, the router goes to the next target and does not cool down the account. `ffmpeg` must be on `PATH`: the router checks this at start when a group sets `convert: "ffmpeg"`.

Diarization needs mono audio. The router cannot know the channel count without decoding, so with `convert: "ffmpeg"` and `diarize=true` it also converts WAV, MP3, and FLAC uploads. Without conversion, the upload goes as it is and Azure answers.

Azure errors (`{"error": {"code": "...", "message": "..."}}`) follow the normal rules: 401 and 403 mark the key invalid, 429 cools down (with `Retry-After`), 5xx and 408 back off, and other 4xx go back to the client.

## Configuration

The config is one JSON file. Default path: `$XDG_CONFIG_HOME/stt-router/config.json`, else `~/.config/stt-router/config.json` (on macOS and Windows, the OS config directory). The state file is `$XDG_STATE_HOME/stt-router/state.json`, else `~/.local/state/stt-router/state.json`.

Flags:

| Flag | Description |
| --- | --- |
| `-config PATH` | Config file path. |
| `-listen HOST:PORT` | Overrides `listen` from the config. |
| `-version` | Print the version and exit. |

Top-level keys:

| Key | Default | Description |
| --- | --- | --- |
| `listen` | `127.0.0.1:8330` | Listen address. |
| `management_key` | generated | Key for the dashboard and `/api/`. `env:NAME` is allowed. |
| `client_keys` | `[]` | `[{"name": "...", "key": "..."}]`. `env:NAME` is allowed for `key`. |
| `state_file` | see above | Path of the state file. |
| `max_upload_mb` | `1024` | Biggest upload. |
| `spool_memory_mb` | `8` | Uploads up to this size stay in memory; bigger ones go to a temp file. |
| `request_timeout` | `"10m"` | Total time for one upstream attempt (Go duration). |
| `quota_cooldown` | `"1h"` | Cooldown after a quota error. |
| `request_log_size` | `500` | Entries in the request log. |
| `frame_ancestors` | `[]` | Origins that can show the dashboard in an `<iframe>`, for example `["http://127.0.0.1:8790"]`. Each value is `http` or `https`, a host, and an optional port, with no path and no wildcard. Empty means no page can frame it. |
| `providers` | | Map from group name to a provider group. |
| `models` | | Map from client model name to a list of targets. |

Provider group:

| Key | Description |
| --- | --- |
| `type` | `openai`, `elevenlabs`, or `azure-speech`. |
| `base_url` | For `openai`, the URL before `/audio/transcriptions`, for example `https://api.groq.com/openai/v1` or `https://api.openai.com/v1`. For `elevenlabs`, `https://api.elevenlabs.io` (the default). For `azure-speech`, the resource endpoint, for example `https://your-resource.cognitiveservices.azure.com`. |
| `convert` | `azure-speech` only. `"off"` (the default) or `"ffmpeg"`. See "Audio formats and `convert`". |
| `accounts` | List of accounts. |

Account:

| Key | Default | Description |
| --- | --- | --- |
| `name` | | Unique in the group. No `/`. |
| `api_key` | | The provider key, or `env:NAME`. |
| `enabled` | `true` | Disabled accounts are never used. |
| `weight` | `1` | An account with weight 2 gets twice the requests of an account with weight 1. |
| `max_concurrent` | `0` (no limit) | Requests in flight on this account. |
| `max_file_mb` | `0` (no limit) | Skip this account for bigger uploads. Use `25` for the Groq free tier. |
| `plain_json` | `false` | The upstream supports only `json` and `text`. |

Target: `{"provider": "<group name>", "model": "<upstream model>"}`.

The router validates the config at start and lists every problem it finds. Unknown keys are errors, so a typo does not go unnoticed.

The router reads the config file only at start. The dashboard writes its changes to the file atomically with mode 0600. If you edit the file by hand, stop the router first, because a later change from the dashboard writes the whole file again.

## Routing and cooldowns

For a request, the router tries the targets of the model in order. For each target it tries the accounts of the group, one at a time, until one succeeds. It never sends the same upload to two accounts at the same time.

An account is usable when it is enabled, not in cooldown, under `max_concurrent`, and the upload is not over its `max_file_mb`. Among usable accounts, the router takes the least recently used one, adjusted by `weight`.

| Upstream result | Action |
| --- | --- |
| 2xx | Success. The backoff counters of the account go back to zero. |
| 429 | Cooldown until the `Retry-After` time (seconds or HTTP date), else the time in a "try again in 1m2.5s" text in the body, else backoff: 30 s, doubling, up to 1 h. Try the next account. |
| 401, 403 | The account is marked `invalid` for 24 h. Try the next account. Use "Reset" in the dashboard after you fix the key. Changing the key also clears it. |
| 402, or ElevenLabs `quota_exceeded` | Cooldown `quota_cooldown` (1 h). Try the next account. |
| 5xx, 408, timeout, network error | Backoff: 5 s, doubling, up to 5 min. Try the next account. |
| other 4xx | Return the error to the client at once. No cooldown, no retry. |

When no account of any target is usable, the answer is 429 with `Retry-After` set to the soonest cooldown end.

The router writes cooldowns, counters, totals, and the request log to the state file. It writes at most every 2 seconds, atomically, and once more at shutdown. On SIGINT or SIGTERM it stops accepting requests, waits up to 60 seconds for requests in flight, and flushes the state.

## Dashboard

Open `http://127.0.0.1:8330/` and sign in with the management key. The page uses one dark theme.

The Routing panel shows the path of a request for one model. Select the model at the top of the panel. The panel remembers the choice in the browser tab.

- Each target of the model is a step. Step 1 is "Preferred"; the next ones are "Fallback 1", "Fallback 2", and so on.
- Each step shows the accounts of its provider group, with state, cooldown countdown, weight, and limits.
- The step that gets the next request has an amber border and the word "next". In that step, the account that the router picks next is marked too. The status API gives this account in the `next` field of each provider group.
- A step with no usable account has a dashed border and a reason, for example "all cooling".
- The last row shows the answer when no account is usable: 429 with `Retry-After`, or 503 when no account is enabled.

To change the order of the steps, drag a step by its handle, or focus the handle and press Arrow Up or Arrow Down. The step menu ("...") has Move up, Move down, and Remove step. "Add step" adds a target at the end. Each change is saved to the config at once. If the save fails, the old order comes back.

The account menu has Enable or Disable, Reset cooldown, Test, and Edit. Accounts in one step are not tried in a fixed order: the router takes the least recently used one, so you cannot drag accounts.

The preview assumes a small upload with no timestamp format. It ignores `max_file_mb` and `plain_json`. It shows busy accounts as they were at the last refresh.

The "Test" button sends a 0.5 second silent WAV to one account, also when the account is in cooldown. A success clears the cooldown; a failure applies the rules above.

## Management API

The dashboard uses these endpoints. They need `Authorization: Bearer <management key>`. No response contains a full provider key or client key; keys are masked to the first 4 and last 4 characters.

| Method and path | Description |
| --- | --- |
| `GET /api/status` | Totals, provider groups with account states and the `next` account, models, client keys. |
| `GET /api/logs` | The request log. |
| `POST /api/providers/{group}/accounts` | Add an account. |
| `PUT /api/providers/{group}/accounts/{name}` | Change an account. Omitted fields keep their values; an empty `api_key` keeps the key. |
| `DELETE /api/providers/{group}/accounts/{name}` | Delete an account. |
| `POST /api/providers/{group}/accounts/{name}/reset` | Clear the cooldown and the invalid mark. |
| `POST /api/providers/{group}/accounts/{name}/test` | Send the test WAV. Optional body `{"model": "..."}`. |
| `PUT /api/models/{name}` | Create or replace a model: `{"targets": [...]}`. |
| `DELETE /api/models/{name}` | Delete a model. |
| `POST /api/client-keys` | Add a client key: `{"name": "...", "key": "..."}`. Without `key`, the router generates one and returns it once. |
| `DELETE /api/client-keys/{name}` | Delete a client key. |

## Security notes

- On a loopback address (`127.0.0.1`, `::1`, `localhost`) with no client keys, clients need no key. On any other address the router refuses to start unless the config has client keys and a management key.
- Keys are compared in constant time.
- The router never logs keys or audio. The request log stores the client key name, not the key.
- If an upstream error text contains the account key, the router masks it before it stores or returns the text.
- The config and state files are written with mode 0600, in a directory with mode 0700.
- The dashboard is one embedded page with no external requests, served with a strict Content Security Policy. The management key is kept in `sessionStorage` of the browser tab.
- By default no page can show the dashboard in a frame. Each origin in `frame_ancestors` can frame it. That origin can then show the dashboard inside its own page, so list only origins that you trust. The framed dashboard still needs the management key.
- The router speaks plain HTTP. To expose it on a network, put a TLS reverse proxy in front of it.

## Performance

The router reads the upload once into a spool: memory up to `spool_memory_mb`, then a temp file. It streams the multipart body to the upstream from the spool, with a known length, for each attempt. Each provider group has one shared HTTP transport with keep-alive and HTTP/2. No lock is held during an upstream call. The temp file is removed after the request, also when the client aborts.

The benchmark in `internal/server/bench_test.go` posts a multipart upload with `response_format=srt` to a fake upstream on loopback, once directly and once through the router. Medians of 3 runs with `make bench` (`-benchtime 3s`), Go 1.25, Intel Core i9-12900KF, Linux, in Docker:

| Upload | Direct to fake upstream | Through the router | Router allocations |
| --- | --- | --- | --- |
| 64 KB | 0.12 ms | 0.48 ms | 250 KB, 402 allocs |
| 1 MB | 0.53 ms | 2.35 ms | 1.25 MB, 407 allocs |
| 16 MB | 9.7 ms | 44.5 ms | 8.9 MB, 423 allocs |

The router overhead is a few tenths of a millisecond plus one extra copy of the upload over loopback, and a temp file write above 8 MB. A real transcription takes seconds, so the overhead is small. The numbers changed by about 2x between runs on this busy desktop; run `make bench` on your own machine.

## Limits

- Only transcription. There is no `/v1/audio/translations` and no streaming responses. The realtime endpoint is turn-based: it is not a token stream.
- The router stores the whole upload before it sends the first byte upstream. This adds the upload time to the latency, and it is what makes retries on other accounts possible.
- One process owns the state. Do not run two routers on the same state file.
- Changes to `listen`, timeouts, and limits need a restart. The dashboard edits accounts, models, and client keys only.
- An upstream response over 64 MB is treated as an error.
- ElevenLabs segments come from a simple rule (sentence end, 1 s silence, speaker change), not from the provider.
- The OpenAI type passes `prompt`; ElevenLabs and Azure Speech have no prompt field, so the router drops it there.
- MAI-Transcribe-2 is a preview model. It takes WAV, MP3, and FLAC only, needs mono audio for diarization, and is offered only in some Speech regions.
- Latency in the dashboard is in whole milliseconds.

## License

MIT. See `LICENSE`.
