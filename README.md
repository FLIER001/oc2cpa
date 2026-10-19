# oc2api — OpenCode free models for CLIProxyAPI

[English](README.md) | [简体中文](README.zh-CN.md)

An external [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin
(c-shared library) that turns **OpenCode Zen free models** into a first-class
CLIProxyAPI provider.

This is a native Go port of the former Node.js oc2api server — the standalone
proxy process and the extra HTTP hop are gone. The plugin runs inside the
CLIProxyAPI host process and talks to the upstream directly.

> 📌 OpenCode keeps shipping free models. `deepseek-v4.1-flash-free` and
> `glm-5.3-flash-free` are the latest, both with a 200,000-token context and
> image input. See [Free models](#free-models).

## What the plugin does

- **OpenAI chat-completions in/out.** Because the executor declares
  `chat-completions` as its input/output format, the host automatically
  translates Claude/Gemini-protocol clients into chat-completions before the
  request reaches this plugin.
- **Reserved gate tool.** The free tier requires a `read` tool. The plugin
  injects it, renames client tools into `oc2api_*` aliases (with `_1`, `_2`
  suffixes on collision), and restores the original names in responses —
  including names split across stream fragments.
- **Stream-only upstream.** The upstream always streams SSE; the aggregate
  executor reassembles it into one JSON response when the client asked for a
  non-streaming request. Streaming answers are passed through frame by frame,
  with tool-call fragment merging and index normalization.
- **Session affinity.** One synthetic `ses_…` session per auth holder, reused
  for 30 minutes, sent on all `x-opencode-session` family headers.
- **Real error semantics.** Only a true upstream rate limit is `429`
  (`rate_limit_error`); parameter problems stay `400`
  (`invalid_request_error`), auth failures stay `401`/`403`
  (`authentication_error`), and plugin-side stream faults use `502`
  (`upstream_stream_error`). Errors are never masked as fake quotas.
- **Watchdog timeouts.** First event within 30 s, body idle within 120 s, and
  a 1 s trailing window after `[DONE]` so late `usage` frames are still
  captured. The connect timeout only bounds dial + response headers — once
  the upstream starts streaming, generation is never cut short.
- **Synthetic auth.** A single `oc2api.json` auth file (auto-created on first
  registration) keeps the plugin usable immediately; optionally set a
  `proxy_url` inside it to give that credential its own exit IP.

## Install

### Plugin store (recommended)

```yaml
plugins:
  enabled: true
  store-sources:
    - https://github.com/zhuweiyou/oc2api/releases/latest/download/registry.json
```

Then install `oc2api` from the CLIProxyAPI management UI.

### Manual

```bash
# linux/amd64 example
unzip oc2api_<version>_linux_amd64.zip     # contains oc2api.so at the zip root
cp oc2api.so /path/to/cliproxyapi/plugins/
```

Platform layout also works: `plugins/linux/amd64/oc2api.so`.

### Configuration

```yaml
plugins:
  enabled: true
  configs:
    oc2api:
      enabled: true
      priority: 100
      upstream_base_url: https://opencode.ai/inference/v1
      connect_timeout_ms: 60000
      dynamic_models: false
```

| Key | Default | Meaning |
| --- | --- | --- |
| `upstream_base_url` | `https://opencode.ai/inference/v1` | Upstream base URL (a local relay can be put here) |
| `connect_timeout_ms` | `60000` | Milliseconds to wait for response headers before giving up (1–600000) |
| `dynamic_models` | `false` | Discover `-free` models from the upstream `/models` endpoint in addition to the built-in catalog |

No API key is needed — the plugin authorizes with the fixed upstream token the
free tier requires.

## Free models

The whitelist keeps `big-pickle` and every model whose id ends with `-free`.
Verified ones:

| Model | Context | Output | Image input |
| --- | --- | --- | --- |
| `big-pickle` | 200,000 | 32,000 | ❌ |
| `mimo-v2.6-flash-free` | 200,000 | 32,000 | ✅ |
| `deepseek-v4.1-flash-free` | 200,000 | 32,000 | ✅ |
| `glm-5.3-flash-free` | 200,000 | 32,000 | ✅ |

Data from [https://models.opencode.ai/api.json](https://models.opencode.ai/api.json)
(`limit` and `modalities`), verify there for new models or set
`dynamic_models: true` to pull them automatically.

## Development

Requires Go 1.26+. Build the shared library for the local machine:

```bash
cd oc2api
go build -buildmode=c-shared -o dist/oc2api.dll .        # Windows
# go build -buildmode=c-shared -o dist/oc2api.so .        # linux
# go build -buildmode=c-shared -o dist/oc2api.dylib .     # darwin
```

Run the offline test suite:

```bash
go test ./...
go vet ./...
```

Cross-platform builds and releases are automated by
[`.github/workflows/build.yml`](.github/workflows/build.yml): a `v*` tag
produces `_linux_amd64`, `_linux_arm64`, `_darwin_amd64`, `_darwin_arm64`,
`_windows_amd64`, and `_windows_arm64` zips plus `checksums.txt` and
`registry.json`.

## History

This project started as a standalone Node.js proxy of the same name
(`npm start` server with local/Docker/Vercel deployment). As of the CLIProxyAPI
plugin era it lives here as a pure Go plugin; the Node implementation was
removed. Kudos to [zhuweiyou](https://github.com/zhuweiyou) — this repo retains
the original project's identity and release channel.
