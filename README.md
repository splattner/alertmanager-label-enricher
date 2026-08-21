# alertmanager-label-enricher

An inline proxy that sits between Prometheus and Alertmanager and enriches
alert labels from external lookups — Kubernetes objects, HTTP endpoints, or
a static file — before forwarding alerts on. Because enrichment happens
before Alertmanager, the new labels participate fully in routing, grouping,
inhibition and silences.

```
Prometheus ──POST /api/v2/alerts──▶ enricher ──┬──▶ alertmanager-0:9093
                                                ├──▶ alertmanager-1:9093
                                                └──▶ alertmanager-2:9093
```

## Why

Prometheus's built-in `alert_relabel_configs` already covers static and
conditional label rewriting. This project exists for the cases that need an
external lookup: e.g. adding a `team` label read off the alert's
`namespace`'s Kubernetes object, or off a CMDB via HTTP.

## Configuration

Point Prometheus's `alerting.alertmanagers` at the enricher instead of
Alertmanager directly, and give the enricher a `config.yaml`:

```yaml
server:
  listen: ":9099"

targets:
  - url: http://alertmanager:9093

sources:
  - name: ns
    type: kubernetes
    kubernetes:
      version: v1
      resource: namespaces
      name: '{{ .Labels.namespace }}'

rules:
  - name: team-from-namespace
    match:
      - { label: namespace, op: exists }
    actions:
      - set:
          label: team
          from: { source: ns, jq: '.metadata.labels["team"]' }
          default: unassigned
```

See [testdata/config.yaml](testdata/config.yaml) for a fuller example
covering all four source types (kubernetes, http, file, and static/
conditional rules with no source at all), and
[docs/configuration.md](docs/configuration.md) for the full reference —
every field, matcher op, action, failure-semantics table, and the
templating/jq mini-languages used inside rules.

### Rule semantics

Rules run in declared order and **all matching rules apply** — a rule is
not skipped because an earlier one already set a label. Later rules see the
labels earlier rules added. Matchers within one rule are ANDed.

Adding a new label is always allowed. Overwriting or dropping an existing
one changes the alert's fingerprint in Alertmanager — which can invalidate
existing silences — so both require `overwrite: true` (on `set`) or an
explicit `drop` action. A `drop` that would leave an alert with no labels
at all is skipped: Alertmanager rejects an unlabelled alert and rejects
the whole batch with it, so the last label is kept to preserve delivery.

`set` and `drop` can target an `annotation` instead of a `label`
(`set: { annotation: runbook_url, value: ... }`). Annotations carry no
fingerprint risk — they're free-form context Alertmanager passes through
to notifications — so overwriting or dropping one needs no extra opt-in.
If you're enriching with something purely human-facing (a runbook link, an
owning Slack channel), prefer an annotation over a label.

**Reserved labels.** `alertname` is Alertmanager's primary identifying
label; overwriting or dropping it is rarely intentional. Config validation
rejects a `set` action with `overwrite: true` or a `drop` action targeting
`alertname` unless it also sets `force: true`:

```yaml
- drop: { label: alertname, force: true }
```

A rule with `required: true` fails the whole batch closed (503, so
Prometheus retries) if its lookup fails and no `default` is set. Every
other rule fails open: forward the alert as-is rather than block delivery.

### Forwarding

```yaml
targets:
  - url: http://alertmanager-0:9093
  - url: http://alertmanager-1:9093
forward:
  minSuccess: 1      # default 1: the batch is delivered once this many targets accept it
  timeout: 5s        # per attempt, per target
  retries: 2         # additional attempts per target after the first, on any failure
```

Each target is tried concurrently; `minSuccess` is how many must accept the
batch before the response to Prometheus succeeds — the rest are left to
finish in the background, independently of the now-completed request (so a
slow replica isn't aborted the instant Prometheus gets its response).
`retries` applies per target, not per batch: a target gets up to `retries`
extra attempts (separated by a short fixed backoff) before it's counted as
failed. The default, `retries: 0`, is a single attempt.

Receiving a batch and forwarding it stay synchronous by design — the
response to Prometheus is the delivery signal it uses for its own
retry/backoff, so acking before forwarding actually succeeds would let
failures go silently undetected. What *is* concurrent: forwarding to every
target (above), and enriching every alert in a batch, bounded by
`enrichment.maxConcurrency` (default 32).

### TLS

Alertmanager itself can serve TLS (via `--web.config.file`), so both sides
of the enricher support it too:

```yaml
server:
  listen: ":9099"
  tls:
    certFile: /etc/enricher/tls/tls.crt
    keyFile: /etc/enricher/tls/tls.key
    clientCAFile: /etc/enricher/tls/ca.crt   # optional: require+verify client certs (mTLS)

targets:
  - url: https://alertmanager:9093
forward:
  tls:
    caFile: /etc/enricher/forward-tls/ca.crt     # trust a self-signed/internal Alertmanager CA
    certFile: /etc/enricher/forward-tls/tls.crt  # optional: present a client cert (mTLS to Alertmanager)
    keyFile: /etc/enricher/forward-tls/tls.key
```

`server.tls` is fixed at startup — a hot reload rotates the certificate,
key, or client CA (useful for cert-manager-style renewal) but cannot turn
TLS on or off without a restart. `forward.tls` is a single shared block, not
per-target: Alertmanager replicas in one cluster normally share the same
server certificate setup.

In the Helm chart, set `tls.server.enabled`/`tls.server.existingSecret` and
`tls.forward.enabled`/`tls.forward.existingSecret` — the paths above
(`/etc/enricher/tls`, `/etc/enricher/forward-tls`) are exactly where it
mounts them, and it switches the liveness/readiness probes to HTTPS
automatically when `tls.server.enabled` is set.

## Running

```sh
go run ./cmd/enricher serve --config config.yaml
go run ./cmd/enricher check --config config.yaml           # validate and exit
go run ./cmd/enricher test  --config config.yaml --alert testdata/alert.json  # before/after label diff
```

`POST /-/reload`, `SIGHUP`, or an edit to the config file all trigger a hot
reload. `/healthz` and `/readyz` are standard Kubernetes probes; `/readyz`
returns 503 until every source (chiefly Kubernetes informers) has completed
its initial sync. Metrics are served at `/metrics`.

## Deploying

The Helm chart ([charts/alertmanager-label-enricher](charts/alertmanager-label-enricher)) is published via chart-releaser to a Helm repo hosted on GitHub Pages:

```sh
helm repo add alertmanager-label-enricher https://splattner.github.io/alertmanager-label-enricher
helm repo update
helm install ale alertmanager-label-enricher/alertmanager-label-enricher \
  --set-file config=config.yaml
```

Use `helm search repo alertmanager-label-enricher --versions` to see available
versions, or pass `--version` to pin one. For chart development, `helm
install`/`helm template` against the local `./charts/alertmanager-label-enricher`
path instead.

If any source has type `kubernetes`, set `rbac.create=true` and list the
resources it needs under `rbac.rules`. Plain manifests are also available
under [deploy/manifests](deploy/manifests) for non-Helm deployments.

To let app teams manage their own enrichment via namespaced
`EnrichmentRule` CRs instead of (or alongside) the file config, set
`crd.install=true` once per cluster (installs the CRD; kept out of the
default install since it's cluster-scoped and `helm uninstall` never
removes it) and `rbac.create=true --set crd.enabled=true` on the release
(adds the RBAC the watch needs). You still need `crd: { enabled: true }`
and an `enforcement` policy in `config`'s own YAML for the enricher to
actually watch and enforce tenancy on them — see
[docs/configuration.md#enrichmentrule-crd-and-tenancy](docs/configuration.md#enrichmentrule-crd-and-tenancy).

`podDisruptionBudget.enabled` and `networkPolicy.enabled` are both off by
default. Turn on `podDisruptionBudget` once `replicaCount` is above 1 — at
the default of 1, a PDB requiring an available pod blocks node drains
outright. `networkPolicy` restricts inbound traffic on the enricher's port
(alert ingestion and `/metrics` share one listener) to
`networkPolicy.ingress.from`, which must be set explicitly — left empty it
denies all ingress. Egress is left unrestricted unless
`networkPolicy.egress` is set, since a safe default would need to know the
cluster's API server, DNS, and any HTTP sources in advance.

Set `serviceMonitor.enabled=true` (Prometheus Operator) to scrape
`/metrics`, and `prometheusRule.enabled=true` for a starter set of alerts
on the enricher itself — down, forward failures, a required rule failing,
config reload failures, high source-lookup error rate. See
[docs/configuration.md](docs/configuration.md#metrics) for what each
underlying metric means. `prometheusRule.labels` is commonly needed to
match your Prometheus Operator's `ruleSelector`.

## Development

```sh
go build ./...
go test -race ./...
go test -tags e2e -run TestE2E ./...   # requires a container runtime (docker or podman)
```
