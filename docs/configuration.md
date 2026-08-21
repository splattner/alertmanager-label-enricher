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
  - [An alert's last label is never dropped](#an-alerts-last-label-is-never-dropped)
  - [Reserved labels](#reserved-labels)
  - [Evaluation order](#evaluation-order)
  - [Failure semantics](#failure-semantics)
- [Templating](#templating)
- [jq extraction](#jq-extraction)
- [Durations](#durations)
- [Environment variable expansion](#environment-variable-expansion)
- [Metrics](#metrics)
- [Reloading](#reloading)
- [EnrichmentRule CRD and tenancy](#enrichmentrule-crd-and-tenancy)

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

Parsing is **strict**: an unrecognised key is an error, not a silent
no-op. A typo like `enrichmnt:` or `retires: 3` would otherwise parse
cleanly, leave the default in place, and give `enricher check` nothing to
report — a config change that looks applied but isn't is a poor failure
mode for the component alert delivery runs through. Everything the enricher
should ignore belongs in a YAML comment.

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
Each of those targets **exactly one** of `label` or `annotation`.

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

  # annotations work the same way, but target `annotation` instead of `label`
  - set:
      annotation: runbook_url
      from: { source: cmdb, jq: '.runbook' }
  - drop: { annotation: description }
```

`set` requires **exactly one** of `value`, `template`, or `from`. Adding a
label/annotation that doesn't yet exist is always allowed. **Overwriting**
one that does requires `overwrite: true`; **dropping** is always an
explicit action already.

For **labels**, both overwrite and drop change the alert's fingerprint in
Alertmanager, which can invalidate existing silences —
`ale_labels_overwritten_total` and `ale_labels_dropped_total` (by rule and
label) exist specifically to make that blast radius observable. For
**annotations**, neither does: annotations are free-form context
Alertmanager passes through to notifications, with no effect on how the
alert is identified. `ale_annotations_added_total` /
`ale_annotations_overwritten_total` / `ale_annotations_dropped_total`
exist purely as informational counters — there's no equivalent guard
needed, and `force` (see below) is rejected on an annotation action as
not applicable rather than silently ignored.

If you find yourself reaching for a label just to carry human-facing
context (a runbook link, an owning Slack channel, a dashboard URL) that
you don't need to route or group on, that's what annotations are for —
using a label for it means paying the fingerprint cost for no benefit.

### An alert's last label is never dropped

Alertmanager refuses an alert carrying no labels at all — and it refuses
the **entire POST** along with it (`at least one label pair required`), so
a single such alert strands every other alert in the batch, from every
other tenant, and Prometheus retries the same payload indefinitely.

So a `drop` that would remove an alert's only remaining label is skipped:
the alert keeps that label and stays deliverable, and the refusal is
counted in `ale_label_drops_refused_total{rule,label}`. This is per-alert,
not per-rule — the same rule drops normally from any alert that has labels
to spare. Annotations have no such constraint; an alert with zero
annotations is perfectly valid.

As a second line of defence, an alert that reaches the forwarding step
with no labels — one that arrived that way, rather than anything a rule
did — is dropped from the batch and counted in
`ale_alerts_dropped_total{reason="no_labels"}` rather than being allowed
to fail delivery for everything alongside it.

### Reserved labels

`alertname` is Alertmanager's primary identifying label. Overwriting it
(`set` with `overwrite: true`) or dropping it (`drop`) is rejected at
config-validation time unless the action also sets `force: true` — a
second, distinct opt-in on top of `overwrite`, since this is rarely
intentional and more consequential than touching an ordinary label. Adding
`alertname` when it's not already present (no `overwrite`) is unaffected.

This guard is label-only. `set: { annotation: alertname, ... }` is legal
and unremarkable — an annotation named `alertname` has no bearing on the
alert's actual identifying label and carries no fingerprint risk.

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

Evaluation is bounded by [`enrichment.timeout`](#enrichment): an
expression still running when the deadline passes is cut short and the
rule fails open (or, for a `required` rule, fails the batch closed). That
bound matters most for jq arriving through an `EnrichmentRule` CR, where
the expression is written by a tenant rather than an admin.

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
| `ale_alerts_dropped_total` | `reason` (`malformed`\|`no_labels`) | individual alerts dropped from an otherwise-forwarded batch — dropping one alert is deliberately preferred to rejecting the batch it arrived in |
| `ale_label_drops_refused_total` | `rule`, `label` | a `drop` skipped because the label was the alert's last one (see [An alert's last label is never dropped](#an-alerts-last-label-is-never-dropped)) |
| `ale_enrichment_panics_total` | — | panics recovered while enriching one alert. **Always a bug**; the alert is forwarded un-enriched rather than taking the process down. Any nonzero value warrants investigation |
| `ale_rule_evaluations_total` | `rule`, `result` (`matched`\|`skipped`\|`required_failed`) | one rule's evaluation against one alert |
| `ale_labels_added_total` | `rule`, `label` | a label newly added |
| `ale_labels_overwritten_total` | `rule`, `label` | an existing label replaced — fingerprint-changing |
| `ale_labels_dropped_total` | `rule`, `label` | a label removed — fingerprint-changing |
| `ale_annotations_added_total` | `rule`, `annotation` | an annotation newly added — informational only, no fingerprint change |
| `ale_annotations_overwritten_total` | `rule`, `annotation` | an existing annotation replaced — informational only |
| `ale_annotations_dropped_total` | `rule`, `annotation` | an annotation removed — informational only |
| `ale_source_lookups_total` | `source`, `result` (`hit`\|`miss`\|`error`) | a source `Lookup()` call: resolved to a value, resolved to nothing, or errored |
| `ale_source_lookup_duration_seconds` | `source` | latency of a source `Lookup()` call (cache hit and cold fetch both included) |
| `ale_forward_duration_seconds` | `target` | latency of one forward attempt to one target |
| `ale_forward_errors_total` | `target` | a target failed after exhausting `forward.retries` |
| `ale_forward_retries_total` | `target` | a retry attempt was made against a target |
| `ale_config_reloads_total` | `result` (`ok`\|`error`) | a config reload attempt, from any trigger (initial load, `SIGHUP`, file watch, `POST /-/reload`) |
| `ale_config_reload_success_timestamp_seconds` | — | unix time of the last successful reload |
| `ale_crd_rules` | `namespace`, `state` (`accepted`\|`rejected`) | `EnrichmentRule` CRs currently known, by namespace |
| `ale_crd_rules_rejected_total` | `namespace`, `reason` (`decode_error`\|`namespace_unreadable`\|`policy_violation`\|`max_rules_exceeded`) | a CR rejected, cumulative |
| `ale_crd_status_updates_total` | `result` (`ok`\|`conflict`\|`error`) | a `status.conditions` write attempt on an `EnrichmentRule` CR. No leader election guards these across replicas, so a nonzero `conflict` rate is expected and benign - see [Rejected rules](#rejected-rules) |

`ale_labels_overwritten_total`/`ale_labels_dropped_total` exist specifically
to make the fingerprint-changing blast radius from [`overwrite`](#actions)
and `drop` observable. `ale_source_lookups_total{result="hit"}` staying
high while your Kubernetes API server's request rate stays flat across
Prometheus's resend cycle is the metric that proves the
[Kubernetes source](#kubernetes-source)'s caching is doing its job in
production — the same claim is exercised in CI by
`internal/proxy/resend_test.go`, which fires an identical alert batch
through the real handler five times (simulating five resend cycles) and
asserts the Kubernetes fake client sees zero API calls and the HTTP
backend sees exactly one request after the informer's initial sync.

## Reloading

The config is re-read, fully re-validated, and only swapped in if valid,
on:

- `SIGHUP`
- the config file changing on disk (fsnotify)
- `POST /-/reload` against the enricher's own listener

A failed reload — parse error, validation error, a new Kubernetes source's
informer failing to sync, or the engine failing to compile — logs the error
and leaves the previously running config serving traffic. It never
partially applies: the replacement generation is built *and* its engine
fully compiled before any of it becomes visible, and the outgoing
generation keeps running (informers included) until the new one is known
good. A generation that fails at any stage is torn down completely.

Initial informer sync is bounded (60s per source, and 60s for the
`EnrichmentRule` watch). If a resource doesn't exist or the ServiceAccount
can't list and watch it, startup fails with an error naming the resource
and the likely cause, rather than blocking forever — an unbounded wait
would leave the pod with no listener, no error and no crash.

## EnrichmentRule CRD and tenancy

Everything above lives in one admin-owned config file. `crd.enabled: true`
adds a second, complementary rule source: namespaced `EnrichmentRule`
custom resources, so an app team with `create`/`update` on
`enrichmentrules` in their own namespace can manage their own enrichment
without a PR against the platform team's config.

```yaml
apiVersion: enricher.splattner.github.io/v1alpha1
kind: EnrichmentRule
metadata:
  name: runbook
  namespace: payments
spec:
  match:   [ { label: severity, op: eq, value: critical } ]
  actions: [ { set: { annotation: runbook_url, value: "https://wiki/payments" } } ]
```

`spec` is the same shape as one entry in `rules` (see [`rules`](#rules)
above), minus `name` (taken from `metadata.name` instead), plus an
optional `order: <int>` for deterministic sequencing among a namespace's
own rules when it has more than one (file-config rules use their position
in the YAML list for this; a CR has no equivalent). The compiled rule name
is `<namespace>/<name>`, so it can never collide with a file-config rule
or another namespace's rule of the same name.

Requires a `kubernetes` source or equivalent RBAC either way: the watch
itself needs `get`/`list`/`watch` on `enrichmentrules` and `namespaces`
(the Helm chart's `crd.enabled=true` adds both automatically — see
[Deploying](../README.md#deploying)).

### The tenancy problem

If any namespace can create an `EnrichmentRule`, what stops namespace A
from rewriting namespace B's alerts? A rule with no `match` at all would
otherwise mutate **every** alert in the stream, from any namespace.
`enforcement` is the file config's answer — evaluated only against
CR-sourced rules; rules declared directly in `rules` are admin-authored
and untouched by it.

```yaml
crd:
  enabled: true
enforcement:
  namespaceMatcherLabel: namespace   # inject `<label> == <CR's own namespace>` on every CR-sourced rule
  rules:                              # first-match-wins on the CR's namespace's own labels
    - namespaceSelector:
        matchLabels: { tenant-isolation: enabled }
      match: [ { label: cluster, op: eq, value: prod } ]   # extra authoritative matchers, ANDed in too
      labels:
        deny: ["severity", "team"]     # allow: [...] also supported - switches to an allow-list, deny ignored
      annotations: {}                  # unrestricted by default - see "Why annotations are unrestricted" below
      allowedSources: ["ns"]           # default: none
      allowRequired: false
      maxRulesPerNamespace: 50
```

| Threat | Control |
|---|---|
| A rule with no `match` mutates every alert in the stream | Injected `<namespaceMatcherLabel> == <CR's namespace>` matcher |
| A tenant rewrites a label the policy asserts — the namespace-scoping label, or one named in the policy's own `match` — so their alert masquerades as another tenant's | Every asserted label is always unwritable by a CR-sourced rule, regardless of `labels.allow`/`labels.deny` |
| Routing escalation — set `severity=critical` or `team=platform` to page someone else's on-call | `labels.deny` (or `labels.allow`) |
| Exfiltration — `from: { source: <shared>, jq: '.data.token' }` copies whatever the enricher's ServiceAccount/credentials can read into a label or annotation, landing in a notification | `allowedSources`, default **none** |
| Availability — `required: true` on a rule that always fails 503s the **whole batch**, not just the tenant's own alerts | `allowRequired`, default **false** |
| Rule flooding | `maxRulesPerNamespace` |
| A tenant's `from.jq` burns CPU without end, starving enrichment of its concurrency slots | jq evaluation is bounded by `enrichment.timeout` |
| A tenant's malformed rule fails the compile, taking every other tenant's rules down with it | CR-sourced rules are fully validated before compilation, in single-tenant mode too; a rule that cannot compile is rejected individually |

A namespace matching **no** `enforcement.rules` entry is fail-closed: CRs
in it are not compiled into the engine at all, logged and counted (see
[Metrics](#metrics)) rather than silently dropped. An entry with no
`namespaceSelector` matches every namespace, so it's usable as a trailing
catch-all after more specific entries.

**`enforcement.rules` left entirely empty is deliberately different**:
with `crd.enabled: true` and no policy at all, every CR-sourced rule
passes through unrestricted, exactly like a file-config rule. This is
single-tenant convenience mode - fine if you trust everyone who can create
an `EnrichmentRule`, and it's what you get by just turning `crd.enabled`
on without also writing an `enforcement` block. The enricher logs a
startup warning naming the exposure so this isn't a silent footgun.

What single-tenant mode does *not* skip is validation. Every CR-sourced
rule is checked for well-formedness — jq and regexes compile, `from.source`
names a declared source, exactly one of value/template/from — before it can
reach the engine, whether or not a policy applies. Trusting who writes a
rule says nothing about whether it parses, and a rule that fails to compile
would fail the recompile for everyone rather than just its author.

### Why every asserted label is unwritable

Matchers are evaluated before actions. A policy that asserts
`cluster == prod` therefore constrains *which* alerts a tenant rule fires
on, but on its own does nothing to stop that rule then setting
`cluster=staging` on the alerts it matched — re-routing production alerts
while satisfying the matcher that was supposed to contain them. So every
label the policy asserts, meaning `namespaceMatcherLabel` plus each label
named in the policy's own `match` list, is unwritable by the rules it
governs: no `set`, no `drop`, regardless of `labels.allow`/`labels.deny`.
Labels the policy says nothing about stay writable, subject to the usual
allow/deny lists.

### Why enforcement only ever adds matchers, never overrides them

A rule's own matchers and the injected ones are combined with plain AND
(the same ANDing every rule's `match` list already does). A tenant rule
that names another namespace doesn't get "fixed" by having its matcher
replaced - it ends up with two matchers on the same label that can never
both be true, so it matches nothing, anywhere. This is simpler and
strictly safer than detecting and overriding a conflicting matcher (which
is what prompted this design's namesake in
[giantswarm/silence-operator#698](https://github.com/giantswarm/silence-operator/pull/698)):
there's no override logic to get wrong, and no risk of a rule silently
applying with a materially different scope than what the tenant wrote.

### Why annotations are unrestricted by default

`labels`/`annotations` are independent policies. Labels default to
deny-nothing-except-the-deny-list because overwriting or dropping one
changes the alert's fingerprint in Alertmanager - the same reason
[reserved labels](#reserved-labels) and `overwrite`/`force` exist for
file-config rules. Annotations carry no such risk (see
[`actions`](#actions)), so there's nothing to protect by default; `labels`
and `annotations` can still be locked down independently via `deny`/`allow`
if a specific deployment wants to.

### Rejected rules

A CR that fails to decode, whose namespace can't be read, or that
enforcement rejects is skipped - logged and counted via
`ale_crd_rules_rejected_total{namespace,reason}` - rather than blocking
compilation of every other rule, from any namespace. A tenant sees this
directly on their own CR, without needing cluster-wide access or the
enricher's own logs:

```console
$ kubectl get enrichmentrule -n payments
NAME      READY   REASON            REQUIRED   AGE
runbook   False   PolicyViolation   false      3m12s

$ kubectl describe enrichmentrule runbook -n payments
...
Status:
  Conditions:
    Type:                 Ready
    Status:               False
    Reason:               PolicyViolation
    Message:              rule "runbook": actions[0]: set label "severity": denied by policy
    Observed Generation:  2
    Last Transition Time: 2026-08-21T09:14:03Z
Events:
  Type     Reason            Age   From                        Message
  ----     ------            ----  ----                        -------
  Warning  PolicyViolation   3m    alertmanager-label-enricher  rule "runbook": actions[0]: set label "severity": denied by policy
```

`status.conditions[type=Ready]` and the `Ready`/`Reason` columns above
come from `kubectl`'s CRD printer columns, driven off the same condition;
a successfully compiled rule shows `Status: True`, `Reason: Compiled`.
Seeing either needs `get` on `enrichmentrules` in your own namespace -
already required to create the CR in the first place - and `create` on
`events` is granted to the enricher's own ServiceAccount by `crd.enabled`
in the chart, not to tenants.

**No leader election.** Every enricher replica evaluates and writes this
status independently - there's deliberately no
`k8s.io/client-go/tools/leaderelection` here, since the standard
`resourcelock.LeaseLock` needs a typed `coordinationv1` client and this
project is dynamic-client-only everywhere else. That trade-off is made
safe rather than exclusive: a status write only happens when the
condition actually changed, and a `Conflict` from a replica that just
wrote the same thing is dropped, not retried - the next debounced
reconcile (which every replica runs on every CR/Namespace change)
converges regardless. The one visible cost is Events: they use
`metadata.generateName` rather than a dedup key, so a multi-replica race
on the same transition can produce a couple of duplicate Event objects.
`ale_crd_status_updates_total{result="conflict"}` is expected to be
nonzero with more than one replica; it climbing relative to `result="ok"`
is the signal that's worth watching, not the raw count.
