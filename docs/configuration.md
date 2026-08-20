# Configuration reference

This is the complete reference for `config.yaml`. For a quick start, see the
[README](../README.md#configuration); for a full working example, see
[testdata/config.yaml](../testdata/config.yaml).

- [Top-level structure](#top-level-structure)
- [`server`](#server)
- [`targets` / `forward`](#targets--forward)
- [`enrichment`](#enrichment)
- [`sources`](#sources)
  - [`kubernetes`](#kubernetes-source)
  - [`http`](#http-source)
  - [`file`](#file-source)
- [`rules`](#rules)
  - [`match`](#match)
  - [`actions`](#actions)
  - [Reserved labels](#reserved-labels)
  - [Evaluation order](#evaluation-order)
  - [Failure semantics](#failure-semantics)
- [Templating](#templating)
- [jq extraction](#jq-extraction)
- [Durations](#durations)
- [Environment variable expansion](#environment-variable-expansion)
- [Metrics](#metrics)
- [Reloading](#reloading)

## Top-level structure

```yaml
server: { ... }       # the enricher's own listener
targets: [ ... ]       # Alertmanager instances to forward to
forward: { ... }       # how targets are fanned out to
enrichment: { ... }    # rule-evaluation budget
sources: [ ... ]       # named lookup sources rules can reference
rules: [ ... ]         # enrichment rules, evaluated in order
```

Only `targets` and `rules` are required; everything else has a default.
Config is YAML (or JSON, since YAML is a superset), loaded once at startup
and re-validated on every [reload](#reloading).

## `server`

```yaml
server:
  listen: ":9099"          # default ":9099"
  maxBodyBytes: 8388608    # default 8 MiB; requests over this get 413
  tls:                     # optional; see the README's TLS section
    certFile: /etc/enricher/tls/tls.crt
    keyFile: /etc/enricher/tls/tls.key
    clientCAFile: ""       # optional; set to require+verify client certs (mTLS)
```

## `targets` / `forward`

```yaml
targets:
  - url: http://alertmanager-0:9093
  - url: http://alertmanager-1:9093

forward:
  minSuccess: 1    # default 1; batch counts as delivered once this many targets accept it
  timeout: 5s       # default 5s; per attempt, per target
  retries: 0        # default 0 (single attempt); extra attempts per target after a failure
  tls:              # optional; see the README's TLS section
    caFile: ""
    certFile: ""
    keyFile: ""
    insecureSkipVerify: false
```

At least one target is required. `minSuccess` must not exceed the number of
targets. All targets are tried concurrently; the response to Prometheus is
sent as soon as `minSuccess` of them accept the batch, and any still
in-flight targets are left running in the background (bounded by their own
timeout/retries, not by the now-completed request) rather than aborted.
`retries` applies per target, separated by a short fixed backoff.

## `enrichment`

```yaml
enrichment:
  timeout: 3s          # default 3s; whole-batch budget for rule evaluation
  maxConcurrency: 32   # default 32; alerts in one batch are enriched concurrently, bounded by this
```

`timeout` bounds every rule evaluation across every alert in a batch —
Prometheus's own Alertmanager timeout is normally 10s, so this (plus
forward timeout/retries) needs to stay comfortably under that. Alerts
within a batch are enriched concurrently, so `maxConcurrency` mainly matters
for large batches hitting slow sources (e.g. an HTTP source without a warm
cache entry).

## `sources`

```yaml
sources:
  - name: ns          # required, referenced from rules as from.source
    type: kubernetes   # kubernetes | http | file
    kubernetes: { ... }
    # http: { ... }
    # file: { ... }
```

Each source has a `name` (unique, referenced by rules) and a `type`, with
exactly the matching block (`kubernetes`, `http`, or `file`) populated.

### Kubernetes source

```yaml
kubernetes:
  group: ""                                 # default "" (core API group)
  version: v1                               # required
  resource: namespaces                      # required, e.g. "namespaces", "pods", "deployments"
  namespace: '{{ .Labels.namespace }}'      # templated; omit entirely for cluster-scoped resources
  name: '{{ .Labels.namespace }}'           # required, templated
```

Backed by a `client-go` dynamic informer (watch-and-cache) over the given
GVR — a lookup is a local map read, not an API call, so there's no
per-lookup latency or API server load, and no TTL to tune. `/readyz`
returns 503 until every configured source (this one included) has
completed its initial sync. `namespace`/`name` render as [templates](#templating); if
either renders to an empty string, the lookup returns "not found" rather
than querying with an empty name. A lookup for an object that doesn't
exist, or a `namespace`/`name` template that renders empty, is "not found"
(not an error) — see [Failure semantics](#failure-semantics) for what
happens next. RBAC for the resource must be granted separately — see the
Helm chart's `rbac.rules`.

### HTTP source

```yaml
http:
  method: GET                                                  # default GET
  url: 'https://cmdb.internal/api/v1/service/{{ .Labels.service | urlquery }}'  # required, templated
  headers:
    Authorization: 'Bearer ${CMDB_TOKEN}'    # ${VAR} expands from env at load — see below
  allowedHosts: ["cmdb.internal"]            # required, at least one host
  timeout: 2s                                # default 2s
  maxResponseBytes: 1048576                  # default 1 MiB; larger responses are an error
  cache:
    ttl: 10m           # default 10m; how long a successful response is cached
    negativeTTL: 30s   # default 30s; how long a failed lookup is cached before retrying
    maxEntries: 10000  # default 10000; oldest-ish entry is evicted arbitrarily once full
```

`url` renders as a [template](#templating); the request is otherwise made
as configured, with **redirects not followed** (a redirect could otherwise
retarget the request to a host outside `allowedHosts`) and the response
body decoded as JSON, fed as-is to the rule's jq query. A `404` response is
treated as "not found" (not an error); any other non-2xx status is an
error. `allowedHosts` is a plain SSRF guard — the rendered URL's host must
be in the list, or the lookup fails closed before any request is made.
Concurrent lookups that resolve to the same method+URL (common within one
batch — several alerts for the same namespace/service) are deduplicated via
singleflight, in addition to being served from cache.

### File source

```yaml
file:
  path: /etc/enricher/teams.yaml   # required; YAML or JSON
```

Loaded once at startup and reloaded whenever the file changes on disk
(watched via fsnotify on the *parent directory*, so a Kubernetes ConfigMap
volume's atomic symlink-swap update is picked up correctly). The whole
parsed document is passed to the rule's jq query — a file source ignores
the alert entirely, so its jq expression is where per-alert lookups happen,
typically via `$labels`/`$annotations` (see [jq extraction](#jq-extraction)),
e.g. `.namespaces[$labels.namespace].team`. An empty or all-null document
is rejected as a load failure (rather than silently becoming "no data") so
a reload racing a non-atomic write can never replace good cached data with
nothing.

## `rules`

```yaml
rules:
  - name: team-from-namespace   # required, unique
    match: [ ... ]              # optional; ANDed matchers, see below
    required: false             # default false; see Failure semantics
    dryRun: false                # default false; compute+log+count but don't mutate
    actions: [ ... ]             # required, at least one
```

### `match`

```yaml
match:
  - { label: namespace, op: exists }
  - { label: team, op: absent }
  - { label: severity, op: eq, value: critical }
  - { label: env, op: ne, value: staging }
  - { label: cluster, op: regex, value: 'prod-.*' }
  - { label: cluster, op: notregex, value: 'prod-.*' }
```

| `op` | Matches when |
|---|---|
| `exists` | the label is present (any value) |
| `absent` | the label is not present |
| `eq` | the label is present and equals `value` |
| `ne` | the label is absent, or present and not equal to `value` |
| `regex` | the label is present and `value` matches its full value |
| `notregex` | the label is absent, or present and `value` does not match its full value |

All matchers in one rule are ANDed. `regex`/`notregex` are **fully
anchored** (compiled as `^(?:value)$`), matching Alertmanager's own
matcher convention — `prod-.*` matches `prod-eu`, not `x-prod-eu-y`. A rule
with no `match` at all always matches (useful for unconditional/static
rules).

### `actions`

Exactly one action per entry: `set` or `drop`, evaluated in list order.

```yaml
actions:
  # literal value
  - set: { label: environment, value: production }

  # Go text/template — see Templating
  - set:
      label: priority
      template: 'P{{ if eq .Labels.severity "critical" }}1{{ else }}3{{ end }}'

  # source lookup via jq, with an optional regex post-step and a fallback
  - set:
      label: tier
      from:
        source: cmdb
        jq: '.data.attributes.tier'
        regex: '^tier-(\d)$'   # optional; first capture group becomes the value, or the whole match if none
      default: unassigned       # used when the lookup/jq/regex yields nothing (not on lookup *errors* unless required:false)
      overwrite: true           # default false; required to replace an existing label
      force: true                # required in addition to overwrite when label is reserved, see below

  # remove a label
  - drop: { label: pod }
  # drop: { label: alertname, force: true }   # reserved labels need force: true
```

`set.label` requires **exactly one** of `value`, `template`, or `from`.
Adding a label that doesn't yet exist is always allowed. **Overwriting**
one that does requires `overwrite: true`; **dropping** a label is always an
explicit action already. Both change the alert's fingerprint in
Alertmanager, which can invalidate existing silences — `ale_labels_overwritten_total`
and `ale_labels_dropped_total` (by rule and label) exist specifically to
make that blast radius observable.

### Reserved labels

`alertname` is Alertmanager's primary identifying label. Overwriting it
(`set` with `overwrite: true`) or dropping it (`drop`) is rejected at
config-validation time unless the action also sets `force: true` — a
second, distinct opt-in on top of `overwrite`, since this is rarely
intentional and more consequential than touching an ordinary label. Adding
`alertname` when it's not already present (no `overwrite`) is unaffected.

### Evaluation order

Rules run in declared order, and **every matching rule applies** — there's
no first-match-wins. A later rule sees labels an earlier rule already
added or changed, so chained enrichment (e.g. resolve `team` from a
Kubernetes namespace, then a second rule maps `team` to an on-call
`escalation` label) works by declaring rules in the right order. This is
the one thing about rule semantics most likely to surprise a new reader.

### Failure semantics

| Situation | `required: false` (default) | `required: true` |
|---|---|---|
| Lookup errors (timeout, non-2xx, RBAC denied, file not loaded) | skip this action, log a warning + `ale_source_lookups_total{result="error"}` | whole batch fails closed: `503`, so Prometheus retries + `ale_rule_evaluations_total{result="required_failed"}` |
| Lookup succeeds but resolves to "not found" (K8s object missing, HTTP `404`, jq/regex yields nothing) and `default` is set | apply `default` | apply `default` |
| Same, but no `default` set | skip this action | whole batch fails closed: `503` |

A rule with `required: true` and a failing action aborts enrichment for the
whole batch immediately — no further rules run, and the batch is never
forwarded. Every other rule fails open: log a warning, skip just that
action, and keep going, so the alert is still delivered (without that one
label) rather than dropped. `dryRun: true` on a rule computes and logs the
would-be result (and still increments matched/skipped/added/overwritten/
dropped metrics) without mutating the alert — the recommended way to roll
out a new rule before trusting it.

## Templating

`url` (HTTP source), `namespace`/`name` (Kubernetes source), and `set.template`
are Go [`text/template`](https://pkg.go.dev/text/template), rendered with:

```go
type Data struct {
    Labels      map[string]string
    Annotations map[string]string
    StartsAt    string
}
```

as the template's `.` — e.g. `{{ .Labels.namespace }}`,
`{{ .Annotations.summary }}`. A missing key renders as an empty string
rather than erroring (`missingkey=zero`). The available functions, beyond
the builtin `text/template` ones:

| Function | Behavior |
|---|---|
| `lower` | lowercase |
| `upper` | uppercase |
| `trim` | trim surrounding whitespace |
| `urlquery` | URL-escape (`net/url.QueryEscape`) — use this on any label interpolated into a URL |
| `default DEF VAL` | `DEF` if `VAL` is empty, else `VAL` |

## jq extraction

`from.jq` is a [gojq](https://github.com/itchyny/gojq) expression run
against the source's lookup result (arbitrary JSON: an object, array,
scalar, or `null`), with the alert's labels/annotations bound as
**`$labels`**/**`$annotations`** — e.g.
`.namespaces[$labels.namespace].team` or
`.metadata.labels["team"]`. Binding values this way, rather than
string-interpolating them into the expression, is injection-safe by
construction.

The result must stringify to a string, number, or boolean (any other
shape — an object, array — is an error). `null`, a missing path, or an
empty string are all treated as "not found", not an error — see
[Failure semantics](#failure-semantics). If `from.regex` is set, it runs
against the stringified jq result; the first capture group becomes the
value if the pattern has one, otherwise the whole match; no match is again
"not found".

## Durations

Every duration field (`forward.timeout`, `enrichment.timeout`,
`http.timeout`, `http.cache.ttl`, `http.cache.negativeTTL`) accepts a
Go-style duration string — `"5s"`, `"2m30s"`, `"1h"` — as well as a raw
number of nanoseconds. Always use the string form in YAML.

## Environment variable expansion

`${VAR_NAME}` anywhere in the raw config file is replaced with that
environment variable's value before YAML parsing — typically used for
secrets in `http.headers` (`Authorization: 'Bearer ${CMDB_TOKEN}'`). An
unset variable is left as the literal `${VAR_NAME}` text rather than
becoming an empty string, so a typo'd name fails loudly (as unexpected
config content, or downstream at the source it's used in) instead of
silently sending an empty header.

## Metrics

Served at `/metrics`, all under the `ale_` prefix:

| Metric | Labels | What it means |
|---|---|---|
| `ale_alerts_received_total` | — | alerts received from Prometheus, before enrichment |
| `ale_alerts_forwarded_total` | `result` (`ok`\|`required_failed`\|`decode_error`\|`forward_failed`) | alert batches, by outcome |
| `ale_rule_evaluations_total` | `rule`, `result` (`matched`\|`skipped`\|`required_failed`) | one rule's evaluation against one alert |
| `ale_labels_added_total` | `rule`, `label` | a label newly added |
| `ale_labels_overwritten_total` | `rule`, `label` | an existing label replaced — fingerprint-changing |
| `ale_labels_dropped_total` | `rule`, `label` | a label removed — fingerprint-changing |
| `ale_source_lookups_total` | `source`, `result` (`hit`\|`miss`\|`error`) | a source `Lookup()` call: resolved to a value, resolved to nothing, or errored |
| `ale_source_lookup_duration_seconds` | `source` | latency of a source `Lookup()` call (cache hit and cold fetch both included) |
| `ale_forward_duration_seconds` | `target` | latency of one forward attempt to one target |
| `ale_forward_errors_total` | `target` | a target failed after exhausting `forward.retries` |
| `ale_forward_retries_total` | `target` | a retry attempt was made against a target |
| `ale_config_reloads_total` | `result` (`ok`\|`error`) | a config reload attempt, from any trigger (initial load, `SIGHUP`, file watch, `POST /-/reload`) |
| `ale_config_reload_success_timestamp_seconds` | — | unix time of the last successful reload |

`ale_labels_overwritten_total`/`ale_labels_dropped_total` exist specifically
to make the fingerprint-changing blast radius from [`overwrite`](#actions)
and `drop` observable. `ale_source_lookups_total{result="hit"}` staying
high while your Kubernetes API server's request rate stays flat across
Prometheus's resend cycle is the metric that proves the
[Kubernetes source](#kubernetes-source)'s caching is doing its job.

## Reloading

The config is re-read, fully re-validated, and only swapped in if valid,
on:

- `SIGHUP`
- the config file changing on disk (fsnotify)
- `POST /-/reload` against the enricher's own listener

A failed reload (parse error, validation error, or a new Kubernetes
source's informer failing to sync) logs the error and leaves the
previously running config serving traffic — it never partially applies.
