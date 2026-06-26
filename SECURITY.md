# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| 0.5.x   | Yes       |
| < 0.5   | No        |

## Reporting a Vulnerability

**Do NOT open a public issue.**

Use one of these channels:

1. **GitHub Security Advisories** (preferred) — use the "Report a vulnerability" button on the [Security tab](https://github.com/TsukumoHQ/WRAI.TH/security/advisories/new)
2. **Email** — security@wrai.th

Please include:

- Description of the vulnerability
- Steps to reproduce
- Affected versions
- Potential impact

### Response Process

| Step | Timeline |
|------|----------|
| Acknowledgement | Within **48 hours** |
| Triage & severity assessment | Within **5 days** |
| Fix development & testing | Within **7 days** of confirmation |
| Coordinated disclosure | After fix is released |

We will credit reporters in the release notes unless anonymity is requested.

## Threat Model

Agent Relay is an **MCP coordination server** designed to run on a local network or behind a reverse proxy. It manages inter-agent messaging, task dispatch, shared memory, and file vault operations backed by SQLite.

**Trust boundaries:**

- The relay trusts its local network — authentication is opt-in
- Agents authenticate via profile slug, not cryptographic identity
- The web UI serves over HTTP by default (TLS via reverse proxy)
- SQLite database is stored on disk with no encryption at rest

**What is NOT in the threat model:**

- Protection against malicious agents with valid access (agents are trusted once connected)
- Encryption of inter-agent messages in transit on the local network
- Multi-tenancy isolation (the relay is single-tenant by design)

## Scope

The following are in scope for vulnerability reports:

- Authentication bypass or privilege escalation
- SQL injection (SQLite)
- Path traversal in vault or static file serving
- Cross-site scripting (XSS) in the web UI
- Denial of service via API abuse
- MCP tool handler input validation issues
- Arbitrary file read/write via vault operations

## Out of Scope

- Exposing the relay to the public internet without authentication is not a supported configuration
- Rate limiting and CORS are opt-in — misconfiguration is not a vulnerability
- Social engineering of agent identities (profile slugs are not secrets)
