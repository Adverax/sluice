# Security Policy

## Reporting a vulnerability

Please report security issues **privately** via GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
(the **Security** tab → "Report a vulnerability"). Do **not** open a public issue for a
suspected vulnerability.

Please include reproduction steps and the affected version/commit. We aim to acknowledge
a report within a few days.

## Scope and intended use

sluice is a **reference / educational** LLM gateway. It demonstrates production patterns,
but it is **not a turnkey production deployment**. Before exposing it to untrusted traffic,
read the README's "What I'd add for production" — in particular:

- **The API key is an identifier only.** In v1 the key is used for rate-limit bucketing and
  usage metering; it is **not validated** against any auth backend. Do not treat possession
  of a key as authentication.
- **Ephemeral keys** are minted for keyless callers to give each its own rate-limit bucket;
  there is no per-source-IP cap on issuance yet (a documented gap).
- The default upstream is an **in-process mock**. Real provider credentials, TLS to the
  collector, secret management, and network policy are deployment concerns left to the operator.

These are intentional v1 boundaries, documented in the README and the ADRs — but they mean
you should add real authentication, authorization, and secret handling before any production
use.

## Supported versions

This is a single-branch reference repository; fixes land on `main`. There is no long-term
support commitment for tagged releases.
