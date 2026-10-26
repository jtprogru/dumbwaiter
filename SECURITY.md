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

## Verifying releases

Every archive, image and chart has a signed build provenance attestation, and the image and the chart are also signed with cosign. The commands are in the notes of each release.
