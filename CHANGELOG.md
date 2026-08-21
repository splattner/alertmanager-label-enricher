# Changelog

## [0.5.0](https://github.com/splattner/alertmanager-label-enricher/compare/v0.4.0...v0.5.0) (2026-08-21)


### Features

* add EnrichmentRule CRD with per-namespace tenancy enforcement ([0a05288](https://github.com/splattner/alertmanager-label-enricher/commit/0a05288e99c33d533d5e49e79fc531e6f5d5d05e))
* add EnrichmentRule CRD with per-namespace tenancy enforcement ([d1eb939](https://github.com/splattner/alertmanager-label-enricher/commit/d1eb939c0efd5197d796912682d9fe62fd9a30a1))
* **crd:** report EnrichmentRule status conditions and Events ([7df4b91](https://github.com/splattner/alertmanager-label-enricher/commit/7df4b9157cf3285aa13fc0b91d9f8e983b7bf778))
* **crd:** report EnrichmentRule status conditions and Events ([5bb9469](https://github.com/splattner/alertmanager-label-enricher/commit/5bb9469addc779344288c95e81f9f213d636f806))


### Bug Fixes

* **chart:** image tag default was missing the v prefix GHCR images use ([c5d4a02](https://github.com/splattner/alertmanager-label-enricher/commit/c5d4a02686fa0fe07ad357429ddeb5aceacfe150))
* **chart:** image tag default was missing the v prefix GHCR images use ([7db3539](https://github.com/splattner/alertmanager-label-enricher/commit/7db35394be8b05ef46fd6fea48881066b32a168a))
* correct lookup caching, action ordering and failure diagnostics ([804af03](https://github.com/splattner/alertmanager-label-enricher/commit/804af036a84739de11c2f43077a580c0c1eeba48))
* correct lookup caching, action ordering and failure diagnostics ([1ef46bd](https://github.com/splattner/alertmanager-label-enricher/commit/1ef46bd9d901dea05936541f0da2babf1f658511))
* **crd:** drop unused ns parameter from maxRulesFor ([56a8682](https://github.com/splattner/alertmanager-label-enricher/commit/56a86824968ae0512ce8e9db6fc06e504b2b74f0))
* **crd:** keep alert delivery independent of CR status writes and rule count ([8634c30](https://github.com/splattner/alertmanager-label-enricher/commit/8634c301a15673a7dc020aa8cadd89ebac4923d3))
* **crd:** keep alert delivery independent of CR status writes and rule count ([ca6566f](https://github.com/splattner/alertmanager-label-enricher/commit/ca6566fc43f46b3e24dd2a009292d1c41513e774))
* **crd:** stop one tenant's rule from taking down everyone else's alerting ([6e5bff1](https://github.com/splattner/alertmanager-label-enricher/commit/6e5bff1d817dfeba4ffd201e69b6d0d23ca6939f))
* **crd:** stop one tenant's rule from taking down everyone else's alerting ([850115f](https://github.com/splattner/alertmanager-label-enricher/commit/850115fb629c234e5a0b7ed5aa9788f6915e7f66))
* stop a malformed alert, a bad reload or missing RBAC from breaking delivery ([c7cfc05](https://github.com/splattner/alertmanager-label-enricher/commit/c7cfc0591f8df0901c3940f5a0fd51202b54f711))
* stop a malformed alert, a bad reload or missing RBAC from breaking delivery ([9b9b4fa](https://github.com/splattner/alertmanager-label-enricher/commit/9b9b4fa3f623e733b095e1ebf2264a3dcfe4ad6e))
* **test:** put context first in awaitReadyCondition ([49478e6](https://github.com/splattner/alertmanager-label-enricher/commit/49478e69a0c9af2a86ba1d5e796cc594472a75ef))
* validate what goes out to Alertmanager, and what comes in from config ([e74b633](https://github.com/splattner/alertmanager-label-enricher/commit/e74b633361e5fc8791f8137b1eda0aac30a48084))
* validate what goes out to Alertmanager, and what comes in from config ([d099fd8](https://github.com/splattner/alertmanager-label-enricher/commit/d099fd8b27956ef9fc947204b730822459d78b21))

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
