# Security Policy

## Supported versions

| Version | Supported |
|---------|-----------|
| latest  | ✅         |
| < 0.8.0 | ❌         |

## Reporting a vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Report security issues privately via GitHub's
[Security Advisory](https://github.com/x7even/cloudcostmcp/security/advisories/new)
feature, or email x7sima@gmail.com with the subject line `[SECURITY] OpenCloudCosts`.

Include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (optional)

You can expect an acknowledgement within 48 hours and a fix or mitigation plan within 14 days.

## Scope

This project accesses cloud pricing APIs. When credentials are configured:
- AWS: Cost Explorer, Savings Plans, Pricing APIs
- GCP: Cloud Billing catalog
- Azure: Retail Prices API (no credentials needed)

Vulnerabilities that could expose billing data, credentials, or allow unauthorized API
access are in scope. See the Security section of [README.md](README.md) for operational
guidelines.
