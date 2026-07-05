# Security Policy

Tollgate sits in the credential path between AI agents and LLM providers —
we take vulnerabilities in it seriously.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Report privately via
[GitHub Security Advisories](https://github.com/opslync/tollgate/security/advisories/new)
("Report a vulnerability" on the repo's Security tab). You'll get an
acknowledgement within a few days, and credit in the advisory once fixed
unless you prefer otherwise.

In scope, especially:

- Agent-key or provider-key leakage (to upstreams, logs, or other agents)
- Budget/kill-switch enforcement bypasses
- Auth bypasses on `/usage` or `/admin` endpoints
- SQL injection through query parameters

## Supported versions

Pre-1.0: only the latest release receives fixes.
