# Keycloak & OIDC Integration Guide

This document provides a step-by-step operational guide for setting up and integrating **Keycloak** (or any OpenID Connect / OIDC provider such as Auth0, Okta, Google Workspace, or Microsoft Entra ID) with **cerbix**.

---

## 🏛️ Architecture Overview

cerbix follows a **Provider-Agnostic OIDC Authentication (AuthN)** model (Decision Records `D-0043` & `D-0045`):

- **Authentication (AuthN)**: Handled externally by Keycloak via **OIDC Authorization Code Flow with PKCE**.
- **User Provisioning**: Just-In-Time (JIT) creation upon first login, keyed uniquely on the `oidc_sub` JWT claim (`D-0044`).
- **Authorization (AuthZ)**: Handled internally by cerbix's role-based access control matrix (`authz.Can(user, action, scope)`).
- **Session Management**: Upon successful OIDC callback, cerbix issues an `HttpOnly`, `SameSite=Lax` session cookie containing a cryptographically hashed token (`session_token`).

```text
[ Browser / Client ] ──1. Click "Continue with SSO"──► [ cerbix API ]
         │                                                    │
         │◄──2. 302 Redirect to Keycloak (PKCE + State)───────┘
         │
         ├──3. Authenticate with Credentials / 2FA ─────────► [ Keycloak ]
         │                                                    │
         │◄──4. 302 Redirect /auth/callback?code=...──────────┘
         │
         └──5. POST /auth/callback (Code) ──────────────────► [ cerbix API ]
                                                              │
                                                              ├── Exchange Code -> Tokens
                                                              ├── Provision User (oidc_sub)
                                                              └── Set Session Cookie
```

---

## ⚙️ Keycloak Side Configuration (Detailed Step-by-Step)

### Option A: Manual Setup via Keycloak Admin Console

#### Step 1: Create a Realm
1. Log in to your Keycloak Admin Console (e.g., `http://localhost:8081` using admin credentials `admin` / `7362` or your production admin credentials).
2. In the top-left realm dropdown menu, click **Create Realm** (or **Add Realm**).
3. Set **Realm Name**: `cerbix`
4. Ensure **Enabled**: `ON`.
5. Click **Create**.

---

#### Step 2: Create and Configure the OIDC Client
1. In the left sidebar under `cerbix` realm, click **Clients** → **Create client**.
2. **General Configuration**:
   - **Client type**: `OpenID Connect`
   - **Client ID**: `cerbix`
   - **Name**: `cerbix Monitoring`
   - **Description**: `OIDC Client for cerbix Uptime & SLA Monitoring`
3. Click **Next**.

4. **Capability Config**:
   - **Client authentication**: `ON` (Confidential Client — produces a client secret).
   - **Authorization**: `OFF`
   - **Authentication flow**:
     - [x] **Standard flow** (Authorization Code Flow) — **REQUIRED**
     - [x] **Direct access grants** (Resource Owner Password Flow) — *Optional*
     - [x] **Service accounts roles** (Client Credentials Grant) — *Required if using OIDC service-account tokens for cerbix API*
     - [ ] *Implicit flow* — **UNCHECK / DISABLED**
5. Click **Next**.

6. **Access Settings**:
   - **Root URL**: `http://localhost:8080` (or `https://cerbix.example.com`)
   - **Home URL**: `http://localhost:8080`
   - **Valid redirect URIs**:
     - `http://localhost:8080/auth/callback`
     - `https://cerbix.example.com/auth/callback`
     - `http://127.0.0.1:8080/auth/callback`
   - **Valid post logout redirect URIs**:
     - `http://localhost:8080/*`
     - `https://cerbix.example.com/*`
   - **Web origins**:
     - `http://localhost:8080`
     - `https://cerbix.example.com`
     - `+` (Allows origins matching valid redirect URIs)
7. Click **Save**.

---

#### Step 3: Copy Client Credentials (Secret)
1. Go to the **Credentials** tab of the `cerbix` client.
2. Under **Client Authenticator**, verify `Client Id and Secret` is selected.
3. Copy the **Client Secret** (e.g., `a1b2c3d4-e5f6-7890-1234-56789abcdef0`).
4. Save this secret — you will set it as `auth.oidc.client_secret` in cerbix's `config.yaml`.

---

#### Step 4: Configure Client Scopes & Claim Mappers (Deep Dive)

##### Why Protocol Mappers are Required
When cerbix exchanges the OIDC authorization code for JWT tokens at `/auth/callback`, it parses the **ID Token** and queries the **UserInfo endpoint**. cerbix reads the following specific JSON claims:

- `sub` (Subject): The user's immutable Keycloak ID (e.g., `a1b2c3d4-e5f6-7890-1234-56789abcdef0`), stored as `oidc_sub` in PostgreSQL.
- `email`: User's primary email address. Required for user identification.
- `email_verified`: Boolean indicating if the email is confirmed.
- `preferred_username`: Display username in cerbix header.
- `name` / `given_name`: User's display full name.

If Keycloak does not map user attributes into the JWT ID Token, cerbix will fail with `400 Bad Request (missing required claim: email)`.

---

##### Step 4.1: Assign Default Client Scopes
1. In Keycloak Admin, go to **Clients** → Click **`cerbix`**.
2. Go to the **Client scopes** tab.
3. Ensure the following scopes are assigned under **Default**:
   - `openid` (Mandatory OIDC base scope)
   - `profile` (Provides `name`, `family_name`, `given_name`, `preferred_username`)
   - `email` (Provides `email`, `email_verified`)

> **Note**: If `email` or `profile` are listed under *Optional*, select them, click **Change type**, and set them to **Default**.

---

##### Step 4.2: Configure Dedicated Client Protocol Mappers
To ensure custom or specific user attributes are always included in tokens issued to cerbix:

1. Go to **Clients** → **`cerbix`** → **Client scopes** tab.
2. Click on the dedicated client scope link: **`cerbix-dedicated`** (or **Configure dedicated mappers**).
3. Click **Add mapper** → **By configuration**.

Add/verify each of the 4 core protocol mappers below:

---

###### Mapper 1: Email Attribute (`email`)
- **Mapper type**: Select `User Attribute`
- **Name**: `email`
- **User Attribute**: `email`
- **Token Claim Name**: `email`
- **Claim JSON Type**: `String`
- Switch Toggles:
  - [x] **Add to ID token**: `ON` (Mandatory for cerbix)
  - [x] **Add to access token**: `ON`
  - [x] **Add to userinfo**: `ON`
- Click **Save**.

---

###### Mapper 2: Email Verified (`email_verified`)
- **Mapper type**: Select `User Attribute`
- **Name**: `email verified`
- **User Attribute**: `emailVerified`
- **Token Claim Name**: `email_verified`
- **Claim JSON Type**: `boolean`
- Switch Toggles:
  - [x] **Add to ID token**: `ON`
  - [x] **Add to userinfo**: `ON`
- Click **Save**.

---

###### Mapper 3: Preferred Username (`preferred_username`)
- **Mapper type**: Select `User Property` (or `User Attribute`)
- **Name**: `username`
- **Property** / **User Attribute**: `username`
- **Token Claim Name**: `preferred_username`
- **Claim JSON Type**: `String`
- Switch Toggles:
  - [x] **Add to ID token**: `ON`
  - [x] **Add to userinfo**: `ON`
- Click **Save**.

---

###### Mapper 4: Full Display Name (`name`)
- **Mapper type**: Select `User's full name`
- **Name**: `full name`
- **Token Claim Name**: `name`
- **Claim JSON Type**: `String`
- Switch Toggles:
  - [x] **Add to ID token**: `ON`
  - [x] **Add to userinfo**: `ON`
- Click **Save**.

---

##### Step 4.3: Testing Token Claims with Keycloak Evaluator
You can test and preview the exact JSON ID Token generated by Keycloak before logging in through cerbix:

1. In Keycloak Admin, go to **Clients** → **`cerbix`** → **Client scopes** tab.
2. Click the **Evaluate** sub-tab.
3. In the **User** field, search for and select your test user (e.g., `testuser@example.com` or `john.doe`).
4. Click **Evaluate**.
5. Select the **Generated ID Token** tab. Verify that the generated JSON contains all required claims:

```json
{
  "exp": 1722816000,
  "iat": 1722812400,
  "iss": "http://localhost:8081/realms/cerbix",
  "aud": "cerbix",
  "sub": "a1b2c3d4-e5f6-7890-1234-56789abcdef0",
  "azp": "cerbix",
  "email": "john.doe@company.com",
  "email_verified": true,
  "preferred_username": "john.doe",
  "name": "John Doe",
  "given_name": "John",
  "family_name": "Doe"
}
```

If `sub` and `email` are present in this JSON preview, Keycloak is 100% properly configured for cerbix!

---

#### Step 5: Create Users & Test Accounts
1. In the left sidebar, click **Users** → **Add user**.
2. Fill in:
   - **Username**: `john.doe`
   - **Email**: `john.doe@company.com`
   - **First Name**: `John`
   - **Last Name**: `Doe`
   - **Email Verified**: `ON`
   - **User Enabled**: `ON`
3. Click **Create**.
4. Go to the **Credentials** tab for `john.doe`:
   - Click **Set Password**.
   - Password: `Password123!`
   - **Temporary**: `OFF` (So Keycloak doesn't force a password change on first dev login).
   - Click **Save** → **Set Password**.

---

### Option B: 1-Click Import via `cerbix-realm.json`

Instead of manual UI configuration, you can import this complete realm configuration into Keycloak:

1. Save the JSON content below as `docker/keycloak/cerbix-realm.json`.
2. In Keycloak Admin Console: **Realm Settings** → top right **Action** menu → **Partial Import** (or create realm from file).
3. Select `docker/keycloak/cerbix-realm.json`, check **Import users** and **Import clients**, and click **Import**.

```json
{
  "realm": "cerbix",
  "enabled": true,
  "registrationAllowed": false,
  "registrationEmailAsUsername": true,
  "rememberMe": true,
  "verifyEmail": false,
  "loginWithEmailAllowed": true,
  "duplicateEmailsAllowed": false,
  "clients": [
    {
      "clientId": "cerbix",
      "name": "cerbix Monitoring",
      "description": "OIDC Client for cerbix Uptime & SLA Monitoring",
      "enabled": true,
      "clientAuthenticatorType": "client-secret",
      "secret": "secret",
      "redirectUris": [
        "http://localhost:8080/auth/callback",
        "https://cerbix.example.com/auth/callback",
        "http://127.0.0.1:8080/auth/callback"
      ],
      "webOrigins": [
        "http://localhost:8080",
        "https://cerbix.example.com"
      ],
      "standardFlowEnabled": true,
      "implicitFlowEnabled": false,
      "directAccessGrantsEnabled": true,
      "serviceAccountsEnabled": true,
      "publicClient": false,
      "protocol": "openid-connect",
      "fullScopeAllowed": true
    }
  ],
  "users": [
    {
      "username": "testuser@example.com",
      "email": "testuser@example.com",
      "firstName": "Test",
      "lastName": "User",
      "enabled": true,
      "emailVerified": true,
      "credentials": [
        {
          "type": "password",
          "value": "password",
          "temporary": false
        }
      ]
    }
  ]
}
```

---

## ⚙️ cerbix Side Configuration (`config.yaml`)

Edit your cerbix configuration file (e.g. `docker/config.example.yaml` or `config.yaml`):

### Docker Compose Static IP Map (`subnet: 10.5.0.0/16`)

When running in Docker Compose with static IP addresses:
- **MariaDB (Keycloak DB)**: `10.5.0.12:3306`
- **Keycloak Server**: `10.5.0.13:8080` (mapped to host `8081`)
- **cerbix Monolith**: `10.5.0.14:8080` (mapped to host `8080`)

```yaml
auth:
  local_enabled: true
  oidc:
    enabled: true
    issuer: "http://10.5.0.13:8080/realms/cerbix"  # Direct static IP of Keycloak
    client_id: "cerbix"
    client_secret: "secret"
    redirect_url: "http://localhost:8080/auth/callback"
    scopes:
      - "openid"
      - "profile"
      - "email"
    button_label: "Continue with Corporate SSO"
```

---

## 🐳 Step 4: Quickstart with Docker Compose (Local Dev)

The repository provides a pre-configured Keycloak container in `docker/docker-compose.yml`:

```bash
# Start the full dev stack (PostgreSQL, RabbitMQ, Keycloak, cerbix)
make dev-init  # only for a fresh broker volume
make dev-up
```

### Dev Environment Pre-sets:
- **Keycloak Admin URL**: `http://localhost:8081`
- **Keycloak Admin Credentials**: `admin` / `7362`
- **Realm**: `cerbix`
- **Client ID**: `cerbix`
- **Client Secret**: `secret`
- **Pre-created Test User**: `testuser@example.com` / `password`

You can immediately test SSO by opening `http://localhost:8080` and clicking **"Continue with Corporate SSO"**.

---

## 🛠️ Production Hardening & Troubleshooting

### 1. Reverse Proxy & TLS (HTTPS)
When deploying Keycloak and cerbix behind Nginx / Traefik / Ingress:
- Ensure your proxy forwards headers: `X-Forwarded-Proto: https`, `X-Forwarded-Host`, `X-Forwarded-For`.
- Keycloak issuer URL in `config.yaml` must match the public HTTPS endpoint (e.g. `https://sso.company.com/realms/cerbix`).

### 2. Internal Docker Networking vs Public URLs
If cerbix runs inside Docker and communicates with Keycloak via internal network `http://keycloak:8080`:
- Ensure the OIDC discovery metadata matches. `cerbix` validates `iss` claim in the ID token against `auth.oidc.issuer`.

### 3. User Role Management
- When a user logs in via Keycloak for the first time, they are assigned the default global role (`Viewer`).
- Global Admins or Org Admins can grant elevated roles (`Org Admin`, `Project Admin`, `Editor`) via the cerbix Workspace Switcher & Member Settings (`GET /api/v1/orgs/{id}/members`).

---

## 🔍 Verification & Logs

To verify successful OIDC authentication in cerbix logs:

```text
level=INFO msg="OIDC discovery successful" issuer=http://localhost:8081/realms/cerbix
level=INFO msg="User authenticated via OIDC" oidc_sub=a1b2c3d4-e5f6-7890 email=john.doe@company.com
```

For authentication errors, check:
- `redirect_uri_mismatch`: Check **Valid Redirect URIs** in Keycloak Client settings.
- `invalid_client_secret`: Check `client_secret` in `config.yaml`.
- `invalid_grant` / `clock skew`: Ensure NTP time synchronization between cerbix and Keycloak hosts.
