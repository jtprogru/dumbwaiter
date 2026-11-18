# Security policy

## Supported versions

dumbwaiter is below 1.0, so fixes land only in the latest release. Upgrade to it before reporting, if you can.

## Reporting a vulnerability

Please do not open a public issue. Report privately through GitHub instead: [Security → Report a vulnerability](https://github.com/jtprogru/dumbwaiter/security/advisories/new).

Include the version (`dumbwaiter -version`), the relevant part of the config with secrets removed, and the requests that reproduce the problem. A proof of concept helps a lot.

You should get a first reply within 7 days. Once a fix is released, the issue is published as a GitHub security advisory, with a CVE requested through GitHub, and you are credited unless you ask otherwise.

## Scope

In scope:

- reading objects outside the bucket and prefix configured for a host, or from another site's bucket;
- bypassing the `Host` routing, the `GET`/`HEAD` restriction or the configured response headers;
- leaking S3 credentials or signed requests to clients or logs;
- a crash, a hang or unbounded resource use triggered by a crafted request;
- problems with the release artifacts, their signatures or attestations.

Out of scope:

- bucket policies, credentials and TLS certificates configured by the operator;
- vulnerabilities in dependencies that the code does not reach, as reported by `govulncheck`;
- load that a CDN or rate limiter in front of the proxy is expected to absorb.

## Known limitations

- `quic-go` has no deadline for reading request headers over HTTP/3. `h3_idle_timeout` and `h3_max_incoming_streams` bound a client that opens a stream and never finishes it, but a slow drip of pings on an otherwise idle connection is only cut when the inactivity timeout fires. With `server.https.http3` enabled, front the proxy with a rate limiter or a CDN.
- The admin port (`/healthz`, `/readyz`, `/metrics`) has no authentication. The binary defaults to loopback. Container configurations explicitly bind it to `:9090`; Compose publishes it only on host loopback. The Helm NetworkPolicy is enabled by default and allows admin ingress only from `metricsFrom` (empty means no pod ingress). Kustomize allows the monitoring namespace. These policies require an enforcing CNI and can be widened by other policies; validate access in the target cluster.
- Global and per-site in-flight limits reject excess work with 503, and a shared lookup timeout bounds S3 fallback. They do not cap incoming connections or requests per second. Use edge rate limiting and restrict direct origin access. `directory_redirect: false` removes an optional S3 lookup per slashless miss; no built-in cache is provided.

## Verifying releases

Every archive, image and chart has a signed build provenance attestation, and the image and the chart are also signed with cosign. The commands are in the notes of each release.
