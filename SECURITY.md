# Security Policy

## Reporting a vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Use GitHub's private [Report a vulnerability](https://github.com/badhrinadhbade/pos-ticker/security/advisories/new) feature instead. This keeps the details private until a fix is available.

Include:
- A description of the vulnerability
- Steps to reproduce it
- What an attacker could do with it
- Your suggested fix (optional but appreciated)

You'll get a response within 48 hours. If the issue is confirmed, a fix will be released as soon as possible and you'll be credited in the release notes.

## Scope

In scope:
- `printer_server.py` — the FastAPI application
- `static/index.html` — the admin UI
- `setup.sh`, `Dockerfile`, `docker-compose.yml` — deployment files

Out of scope:
- Vulnerabilities in third-party dependencies (report those upstream)
- Issues that require physical access to the device
- Denial of service via the public `/contact` endpoint (rate limiting is intentional)

## Security design notes

A summary of security decisions already made is in [README.md § Security & public deployment](README.md#security--public-deployment).
