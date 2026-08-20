# Changelog

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
