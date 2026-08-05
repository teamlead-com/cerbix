# Security Policy

The **cerbix** team takes the security of our project and the privacy of our users very seriously. We appreciate your help in keeping cerbix safe and secure for everyone.

---

## 🛡️ Supported Versions

We actively provide security updates and patches for the following versions of cerbix:

| Version | Supported          |
| ------- | ------------------ |
| `v1.x`  | :white_check_mark: |
| `< v1.0`| :x:                |

---

## 📩 Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub Issues.**

If you discover a security vulnerability or suspect an issue in cerbix (including SSRF, authentication bypass, secret exposure, or privilege escalation), please report it privately:

- **Email**: Send an email to `security@example.com`.
- **Encryption**: If desired, request our PGP public key in your initial outreach.

### What to Include in Your Report

To help us triage and resolve the issue quickly, please include as much information as possible:

1. **Type of Issue**: (e.g. SSRF, Auth Bypass, SQL Injection, Secret Exposure, Rate-Limiting Bypass).
2. **Affected Component**: (e.g. `api`, `auth`, `prober`, `outbox`, `agent`, `web`).
3. **Step-by-step Reproduction**: Clear instructions or a proof-of-concept (PoC) script.
4. **Impact Analysis**: What an attacker could achieve by exploiting this vulnerability.
5. **Mitigation**: Any potential fix or workaround you've identified.

---

## ⏱️ Response Time & SLA

When you submit a security report:

- **Acknowledgment**: You will receive an initial response acknowledging your report within **24 hours**.
- **Triage**: We will assess the severity and impact within **72 hours**.
- **Patch & Fix**: Critical security vulnerabilities will be patched within **7 days** with an advisory and release update.

---

## 🔒 Security Best Practices in cerbix

For reference, cerbix enforces the following security invariants in the codebase:

- **At-Rest Encryption**: Webhook secrets and channel credentials are encrypted using AES-256-GCM ([`secret.Cipher`](../backend/internal/secret/secret.go)) with zero-downtime key rotation (`cerbix reencrypt`).
- **SSRF Guard**: Prober targets are validated against resolved IP addresses ([`prober.Guard`](../backend/internal/prober/guard.go)), blocking cloud metadata (`169.254.169.254`) and link-local ranges by default.
- **Tenant Isolation**: Every SQL query filters data access at the database boundary (`org_id`, `project_id`) enforced by `authz.Can()`.
- **Password Security**: Local passwords are hashed with Argon2id and guarded against brute-force attacks by sliding-window rate limiters.
- **No Secret Logging**: Bearer tokens, passwords, session cookies, and API keys are strictly excluded from structured logs (`slog`).
