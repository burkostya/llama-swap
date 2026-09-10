---
title: Session-backed llama.cpp slots
summary: Configure compile-time extensions to identify sessions and save or restore llama.cpp slot state across model swaps.
category: guides
tags: [sessions, extensions, llama-cpp, cache]
config_keys: [models.*.extensions]
updated: 2026-09-10
---

# Session-backed llama.cpp slots

This fork compiles extension implementations into `llama-swap` and composes
them per model from YAML. It does not load Go plugins, shared libraries, WASM,
or extension processes.

## Configuration

`session.identity.v1` derives an anonymized session key. The
`llamacpp.session-state.v1` consumer serializes requests for that session,
assigns a llama.cpp slot, saves an LRU victim, lazily restores an existing
snapshot, and adds `id_slot` and `cache_prompt` to the JSON request.

```yaml
models:
  qwen:
    cmd: >-
      llama-server --model qwen.gguf --port ${PORT}
      --slots --slot-save-path /cache/qwen
    extensions:
      - name: session
        type: session.identity.v1
        config:
          sources:
            - header: X-Session-ID
            - jsonPath: metadata.conversation_id
          tenantSources:
            - header: X-Tenant-ID
          hmacSecret: ${env.LLAMA_SWAP_SESSION_SECRET}

      - name: kv-session
        type: llamacpp.session-state.v1
        requires:
          - session.identity.v1
        failurePolicy: fail
        readyTimeout: 30s
        stopTimeout: 30s
        config:
          persistencePath: /cache/qwen
          eviction: lru
          maxQueue: 128
          queueTimeout: 30s
```

`persistencePath` must be the same local directory passed to llama-server as
`--slot-save-path`. The extension verifies that a file saved through the slot
API is visible there before considering the snapshot durable.

The identity extension manages these paths by default:

- `/v1/chat/completions`
- `/v1/responses`
- `/v1/completions`
- `/v1/messages`
- `/completion`
- `/infill`

Set `config.paths` to replace that list. A trailing `*` performs prefix
matching. Requests on other paths pass through without session management.
The default `missingPolicy` is `reject`; `ephemeral` creates a one-request
identity and `passthrough` leaves the request unmanaged.

## Control API

The normal API-key middleware protects the generic extension namespace:

```text
GET    /api/extensions
GET    /api/extensions/{model}/{extension}/sessions
GET    /api/extensions/{model}/{extension}/resources
POST   /api/extensions/{model}/{extension}/sessions/{key}/suspend
DELETE /api/extensions/{model}/{extension}/sessions/{key}
```

Session keys returned by this API are HMAC-SHA256 values. External session IDs
and tenant IDs are never returned or written to request metadata.

## Lifecycle and failures

Extension instances live for the lifetime of one model process object and may
observe multiple backend generations. Ready hooks and HTTP wrappers run in
configuration order; stopping and close hooks run in reverse order. The
process stops accepting new requests before the blocking stopping hooks drain
active streaming responses and save state.

`failurePolicy` may be `fail`, `disable`, or `warn` for backend-ready hooks.
Stopping always continues after hook errors, timeouts, or panics. Session-state
should use `fail`, because accepting unmanaged requests after initialization
failure can mix slot state between sessions.

Extension-specific `config` is opaque to core. `${env.VAR}` substitution still
applies before YAML decoding; core macros such as `${PORT}` and `${MODEL_ID}`
are intentionally not expanded inside this block. Factories receive the
resolved model configuration and runtime backend URL instead.

The `globalConcurrencyLimit` still applies before session handling. It rejects
excess requests with 429; it does not replace the per-session queue that prevents
two requests from using the same slot concurrently. Cancelling a request releases
its session lease without restarting the backend.
