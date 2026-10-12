---
title: Exclusive resources and polling workers
summary: Start generic polling workers from demand heartbeats and hand exclusive resources over only after confirmed graceful shutdown.
category: guides
tags: [resources, gpu, worker, queue, demand, graceful, shutdown]
config_keys: [routing.router.use, routing.router.settings.resources, models.*.activation, models.*.resources, models.*.drain]
updated: 2026-10-12
---

# Exclusive resources and polling workers

The Linux `resources` router supervises foreground backends that share named
exclusive resources. Resource parameters are administrator-defined strings,
not detected hardware or a guarantee of GPU isolation. Every conflicting
backend must participate in this one supervisor.

```yaml
routing:
  router:
    use: resources
    settings:
      resources:
        stateDir: /var/lib/llama-swap/resources
        demandTimeout: 30s
        pools:
          gpu0:
            parameters: {device: GPU-uuid-0}
          gpu1:
            parameters: {device: GPU-uuid-1}
models:
  worker-a:
    cmd: worker-a --port ${PORT}
    activation: demand
    resources: [gpu0]
    env: ["WORKER_GPU=${RESOURCE:gpu0.device}"]
    drain: {signal: SIGTERM, target: process, timeout: 120s, onTimeout: block}
  worker-b:
    cmd: worker-b --port ${PORT}
    activation: demand
    resources: [gpu0, gpu1]
```

Each backend's complete resource set is reserved before start. Disjoint sets
run concurrently. `${RESOURCE:name.parameter}` works in `cmd`, `cmdStop` and
`env`; command substitution happens after argv parsing. References to
unassigned resources or missing parameters fail configuration validation.

`activation` defaults to `request` (HTTP-triggered), `demand` permits only
external demand, and `both` permits either. A demand-only worker's health/status
polling does not start it. Group/matrix rules are inactive in this routing mode.
Existing configs without resources keep their original semantics.

## Report work

The main application owns its job queue. Every 10 seconds it reports whether
there is work for a configured backend:

```bash
curl --fail-with-body -X PUT http://127.0.0.1:8080/api/resources/demand \
  -H 'Content-Type: application/json' \
  --data '{"model":"worker-a","source":"service-a","hasWork":true}'
```

Use the configured API authentication when enabled. This request returns
immediately with `model`, `generation`, `state`, `queuePosition`, `resources`,
`sources` (expiry timestamps), and `inflight`. Repeat `true` to renew one source;
it does not add queue entries. Send `false` to withdraw that source. Multiple
sources are combined. An expired heartbeat withdraws demand; it does not prove
that a running worker released the resource.

A new conflicting waiter closes the current owner's admission, waits for
already forwarded HTTP requests, then requests graceful shutdown. There is no
quantum or minimum residence window. The worker stops polling for new jobs,
finishes accepted work, releases its devices, and exits. FIFO (with existing
per-model priorities) orders activations, not individual jobs; it does not
guarantee a number of jobs per activation. Workers must handle drain arriving
immediately after startup. After a victim exits, only a fresh `true` report or
new HTTP activation requeues it; old demand cannot create a restart loop.

## Graceful completion and troubleshooting

The default signal is SIGTERM to the main process, leaving child calculations
alive. If `cmdStop` is configured it replaces the signal. Returning from
`cmdStop` is only acknowledgment, not proof of shutdown. Use foreground
commands; container/service wrappers must wait for the actual backend to exit.
Daemonizing or escaping the managed process group is unsupported.

After the drain timeout (default 120s), the backend is `blocked`. No automatic
SIGKILL occurs. The process and its remaining non-zombie group members must
exit before a competitor starts. Startup failures, TTL, manual unload and
supervisor shutdown use the same ownership fence. A blocked shutdown can wait
indefinitely; configure the surrounding service manager accordingly.

`GET /api/resources` lists owners, generations, queue, source expirations and
blocking reasons. To explicitly force the current owner to stop:

```bash
curl --fail-with-body -X POST http://127.0.0.1:8080/api/resources/force-stop \
  -H 'Content-Type: application/json' \
  --data '{"model":"worker-a","generation":7}'
```

Use the generation returned by the status API, not a hardcoded value. Wrong
generations are rejected. Force-stop signals the entire owned process group;
it still does not release resources before termination is confirmed.

The private stateDir contains an atomically written ownership ledger and an
exclusive supervisor lock. After a crash, live or unconfirmed owners block
resources; they are never automatically adopted or evicted. Confirmed exits
are reclaimed. An owner with no recorded process identity requires an operator
to verify cleanup before repairing the ledger while the supervisor is stopped.
Do not delete ownership records merely to get an activation unstuck.

Reload is rejected while owners or queued work exist. Changes to/from resource
mode require a restart. Empty resource routers support a fenced reload without
two servers admitting work simultaneously. Demand/queued requests are not
persisted; main applications re-report after restart.
