# Changelog

## [0.5.0](https://github.com/splattner/alertmanager-label-enricher/compare/v0.4.0...v0.5.0) (2026-08-21)

This release adds namespaced `EnrichmentRule` custom resources, and is
otherwise a stability pass over the whole project: 18 findings from a
focused audit of how the enricher can stop delivering alerts, each one
reproduced against the code before being fixed and each shipping with a
regression test. The most serious allowed any client that could reach the
listener to terminate the process with a single request.

### ⚠ BREAKING CHANGES

Three changes can break an existing install. None require a code change,
but all three can turn a working config into one that is rejected at
startup — check before upgrading.

* **Config parsing is now strict.** An unrecognised key is an error rather
  than being silently ignored. This is deliberate: a typo like
  `enrichmnt:` or `retires: 3` previously parsed cleanly and left the
  default in place, so a config change appeared to apply and did not. Run
  `enricher check --config <file>` against your config before upgrading;
  anything the enricher should ignore belongs in a YAML comment.
* **`/` is now reserved in `rules[].name`.** Rules from an
  `EnrichmentRule` CR compile to `<namespace>/<name>`, so a file-config
  rule containing `/` could silently share a metric series with a
  tenant's. Rename any rule whose name contains `/`.
* **`crd.maxRules` now defaults to `1000`.** The total number of compiled
  CR-sourced rules was previously unbounded. Only affects clusters with
  more than 1000 `EnrichmentRule` objects, where the excess is now
  rejected (and reported on each CR's status). Set `crd.maxRules` higher,
  or `0` for the previous unlimited behaviour.

### Features

* add EnrichmentRule CRD with per-namespace tenancy enforcement ([d1eb939](https://github.com/splattner/alertmanager-label-enricher/commit/d1eb939c0efd5197d796912682d9fe62fd9a30a1))
* **crd:** report EnrichmentRule status conditions and Events ([5bb9469](https://github.com/splattner/alertmanager-label-enricher/commit/5bb9469addc779344288c95e81f9f213d636f806))

### Bug Fixes

* stop a malformed alert, a bad reload or missing RBAC from breaking delivery ([9b9b4fa](https://github.com/splattner/alertmanager-label-enricher/commit/9b9b4fa3f623e733b095e1ebf2264a3dcfe4ad6e))
* **crd:** stop one tenant's rule from taking down everyone else's alerting ([850115f](https://github.com/splattner/alertmanager-label-enricher/commit/850115fb629c234e5a0b7ed5aa9788f6915e7f66))
* validate what goes out to Alertmanager, and what comes in from config ([d099fd8](https://github.com/splattner/alertmanager-label-enricher/commit/d099fd8b27956ef9fc947204b730822459d78b21))
* correct lookup caching, action ordering and failure diagnostics ([1ef46bd](https://github.com/splattner/alertmanager-label-enricher/commit/1ef46bd9d901dea05936541f0da2babf1f658511))
* **crd:** keep alert delivery independent of CR status writes and rule count ([ca6566f](https://github.com/splattner/alertmanager-label-enricher/commit/ca6566fc43f46b3e24dd2a009292d1c41513e774))
* **chart:** image tag default was missing the v prefix GHCR images use ([7db3539](https://github.com/splattner/alertmanager-label-enricher/commit/7db35394be8b05ef46fd6fea48881066b32a168a))
* **crd:** drop unused ns parameter from maxRulesFor ([56a8682](https://github.com/splattner/alertmanager-label-enricher/commit/56a86824968ae0512ce8e9db6fc06e504b2b74f0))
* **test:** put context first in awaitReadyCondition ([49478e6](https://github.com/splattner/alertmanager-label-enricher/commit/49478e69a0c9af2a86ba1d5e796cc594472a75ef))

### What the stability pass covers

Grouped by the delivery failure each one caused:

* **The process could be killed remotely.** A single `[null]` in an alert
  batch wrote to a nil map on a goroutine nothing recovered, terminating
  the enricher. Malformed alerts are now dropped individually, and
  enrichment runs under a panic backstop so no one alert can take the
  process down.
* **Startup and reload could fail silently.** A failed reload stopped the
  outgoing generation's informers before the new engine was known good,
  leaving the proxy serving dead sources with `/readyz` still green; and a
  missing CRD or absent RBAC blocked startup forever with no listener, no
  error and no crash. Reloads are now atomic, and informer sync is bounded
  with a diagnosable error.
* **One tenant could break every tenant.** `enrichment.timeout` did not
  actually bound jq, so a tenant expression could pin a core and leak its
  concurrency slot permanently; a malformed CR failed the whole compile and
  stopped the process booting; and labels a policy asserted were still
  writable, letting a rule match on `cluster=prod` and then relabel it.
* **One alert could poison a batch.** A `drop` could strip an alert to zero
  labels, which Alertmanager rejects — rejecting the entire POST with it,
  so Prometheus retried the same undeliverable payload indefinitely.
* **Failures were hard to diagnose.** A client disconnect poisoned an HTTP
  source's cache for 30 seconds; actions within a rule silently ran out of
  declared order; and the message behind a required-rule 503 read `<nil>`.

## [0.4.0](https://github.com/splattner/alertmanager-label-enricher/compare/v0.3.0...v0.4.0) (2026-08-20)


### Features

* **chart:** add optional PrometheusRule with self-monitoring alerts ([5f7f571](https://github.com/splattner/alertmanager-label-enricher/commit/5f7f5716587c7ac22e7e017c9522a6d4c93fa882))
* **chart:** add optional PrometheusRule with self-monitoring alerts ([99244a6](https://github.com/splattner/alertmanager-label-enricher/commit/99244a6a2626ac04b77cff823eed5eced8ab19df))
* support setting/dropping annotations, not just labels ([3b12308](https://github.com/splattner/alertmanager-label-enricher/commit/3b12308cf633a7b4c7a5c6131650534a191fa7ad))
* support setting/dropping annotations, not just labels ([d5326c7](https://github.com/splattner/alertmanager-label-enricher/commit/d5326c7f2d844efeb6cd4ce68a9a620e7b629509))


### Bug Fixes

* wire up source_lookups/config_reload metrics, complete required_failed ([810a68b](https://github.com/splattner/alertmanager-label-enricher/commit/810a68bed9165e5617377ad76a64ccc19a7b90d0))
* wire up source_lookups/config_reload metrics, complete required_failed ([81ab4a8](https://github.com/splattner/alertmanager-label-enricher/commit/81ab4a8f8b2faa3f1f9cb507836562335ba28e66))


### Performance Improvements

* parallelize enrichment and detach forward stragglers from request ctx ([79ebc71](https://github.com/splattner/alertmanager-label-enricher/commit/79ebc716b49e255d12df6e92337be39bea3acdce))
* parallelize enrichment, detach forward stragglers from request ctx ([f02179f](https://github.com/splattner/alertmanager-label-enricher/commit/f02179fc67fe0c6dde11b2f7cb63b69da49063dc))

## [0.3.0](https://github.com/splattner/alertmanager-label-enricher/compare/v0.2.0...v0.3.0) (2026-08-20)


### Features

* **chart:** add optional PodDisruptionBudget and NetworkPolicy ([7e7e938](https://github.com/splattner/alertmanager-label-enricher/commit/7e7e938c7ca01ebbf1904c63cbcdc6dd09dccace))
* **chart:** add optional PodDisruptionBudget and NetworkPolicy templates ([07803f9](https://github.com/splattner/alertmanager-label-enricher/commit/07803f9527264695337f16d21f01b676012f8977))
* guard alertname from silent overwrite/drop ([1f3c819](https://github.com/splattner/alertmanager-label-enricher/commit/1f3c819a526b7fd563d587a39ed322749948a71d))
* guard alertname from silent overwrite/drop ([88d6f4f](https://github.com/splattner/alertmanager-label-enricher/commit/88d6f4fb3bfd0f4881f68b945112fef1a130af95))


### Bug Fixes

* check validates TLS cert/key/CA files ([759a1cb](https://github.com/splattner/alertmanager-label-enricher/commit/759a1cb1f5d636f76f45d0eef13f87f78f8da560))
* implement forward.retries, and fix duration strings not parsing at all ([9533a96](https://github.com/splattner/alertmanager-label-enricher/commit/9533a9616a69829c04d35585ca71e21784b2f78a))
* implement forward.retries, and fix duration strings not parsing at all ([702312c](https://github.com/splattner/alertmanager-label-enricher/commit/702312c7f38c28447c4610b302a0d62afc02b002))
* make check validate TLS cert/key/CA files, not just config shape ([18a8777](https://github.com/splattner/alertmanager-label-enricher/commit/18a87779df360777a144f029003c03aae8d265fb))

## [0.2.0](https://github.com/splattner/alertmanager-label-enricher/compare/v0.1.0...v0.2.0) (2026-08-20)


### Features

* add Helm chart support for TLS ([16c4c4b](https://github.com/splattner/alertmanager-label-enricher/commit/16c4c4b9bf918ce7c527d906087d56380f66cfd8))
* add TLS support for inbound and outbound connections ([3c296be](https://github.com/splattner/alertmanager-label-enricher/commit/3c296bec138fa081ea7592ab6b6bf5c456aca042))
* add TLS support for the inbound listener and outbound forwarding ([e558d0e](https://github.com/splattner/alertmanager-label-enricher/commit/e558d0e580fedbf5952f059ec694f3001a9653dc))
* initial implementation of the alert label enrichment proxy ([c60cedf](https://github.com/splattner/alertmanager-label-enricher/commit/c60cedf4a432047336b586c5ceb9b89adb02589d))


### Bug Fixes

* flaky TestReloadOnWrite panic and harden file source against torn reads ([1de80b6](https://github.com/splattner/alertmanager-label-enricher/commit/1de80b6dad71d42a2e3e44144957aec9b0f17b55))
* flaky TestReloadOnWrite panic and harden file source against torn reads ([cb54080](https://github.com/splattner/alertmanager-label-enricher/commit/cb54080ad8a6b22d3f6686897a4dcbf446387984))
