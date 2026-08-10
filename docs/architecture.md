# cerbix — System Architecture & Workflow Diagrams

This document provides a comprehensive breakdown of the **cerbix** architecture, system components, data flows, sequence diagrams, and module interactions.

---

## 📐 1. Single Binary Concept & Process Roles

`cerbix` ships as a single compiled Go static binary with embedded database migrations and embedded Vue 3 SPA frontend assets ([`internal/web`](../internal/web)).

Process behavior is determined by the `--role` flag:

```mermaid
flowchart TD
    CLI["cerbix serve --role <role>"]
    
    CLI -->|--role=all| ALL["Role: ALL (Monolith)<br/>API + SPA + Scheduler + Worker + Ingest<br/>Transport: inproc (no RabbitMQ required)"]
    CLI -->|--role=api| API["Role: API / Ingest<br/>REST API + SSE + SPA + Outbox Delivery<br/>Transport: AMQP"]
    CLI -->|--role=scheduler| SCHED["Role: SCHEDULER<br/>Leader (Advisory Lock) + Rollup + Escalations<br/>Transport: AMQP"]
    CLI -->|--role=worker| WRK["Role: WORKER<br/>Stateless Prober Pool (Worker Pool)<br/>Transport: AMQP"]
    CLI -->|--role=agent| AGT["Role: AGENT<br/>HTTP Pull Agent for Isolated Geo Zones<br/>Transport: Outbound HTTPS"]
```

---

## 🏗️ 2. Distributed System Topology

In production, roles are deployed as independent, horizontally scalable containers.

```mermaid
flowchart TB
    subgraph Clients["Clients & External Systems"]
        User["Browser / User"]
        Prom["Prometheus / Alertmanager"]
    end

    subgraph IngressTier["Ingress Tier"]
        LB["Traefik / Ingress Controller (TLS Termination)"]
    end

    subgraph APITier["API Tier (Stateless, N replicas)"]
        API1["cerbix --role api (Node 1)"]
        API2["cerbix --role api (Node 2)"]
    end

    subgraph SchedTier["Scheduler Tier (HA Standby, 1 active)"]
        SCH1["cerbix --role scheduler (Leader)"]
        SCH2["cerbix --role scheduler (Standby)"]
    end

    subgraph WorkerTier["Worker Pools (Stateless, M replicas)"]
        W1["cerbix --role worker --region core"]
        W2["cerbix --role worker --region us-east"]
    end

    subgraph RemoteGeo["Remote Segments (DMZ / Firewalled)"]
        AGT1["cerbix --role agent --region asia-south"]
    end

    subgraph InfraTier["Storage & Bus Infrastructure"]
        MQ[("RabbitMQ 3.12 Cluster<br/>(Exchanges & Queues)")]
        PG[("PostgreSQL 16<br/>(Heartbeats, Rollups, Outbox, Settings)")]
    end

    Clients --> LB
    LB -->|REST / SSE / SPA| API1
    LB -->|REST / SSE / SPA| API2

    API1 & API2 --- MQ
    API1 & API2 --- PG

    SCH1 ---|Advisory Lock Leader Election| PG
    SCH1 & SCH2 --- MQ

    W1 & W2 ---|AMQP checks.jobs / checks.results| MQ

    AGT1 -->|"Outbound HTTPS GET /agent/jobs (Long-Polling)"| API1
    AGT1 -->|Outbound HTTPS POST /agent/results| API1
```

---

## 🔄 3. Monitoring Execution Lifecycle (Main Data Flow)

Below is the sequence diagram illustrating a regular check execution (from scheduling to alert delivery):

```mermaid
sequenceDiagram
    autonumber
    participant S as Scheduler (Leader)
    participant MQ as RabbitMQ
    participant W as Worker
    participant T as Target Service
    participant I as Ingest (API Service)
    participant DB as PostgreSQL
    participant OB as Outbox Worker
    participant N as Notification Channel

    Note over S: Scheduler Tick (Heap next_run)
    S->>MQ: Publish CheckJob (checks.jobs.<region>)
    MQ->>W: Deliver CheckJob (Prefetch)
    
    Note over W: SSRF Guard (guard.go) IP Check
    W->>T: Execute Probe (HTTP/TCP/ICMP/DB/Synthetic)
    T-->>W: Response / Timeout
    
    W->>MQ: Publish Result (checks.results)
    MQ->>I: Deliver Result (Heartbeat)
    
    I->>DB: Insert Heartbeat (Daily Partition)
    I->>DB: SetMonitorStatus (Update status)
    
    alt Status Changed (UP -> DOWN)
        Note over I: Status Flip
        I->>DB: Auto-Create Incident & Write Event to Outbox
        Note over DB: Transaction: Status + Outbox Event
        
        Note over OB: Outbox Worker (SKIP LOCKED)
        OB->>DB: Claim Outbox Event
        OB->>N: Deliver Alert (Telegram/Slack/Webhook/Email)
        N-->>OB: 200 OK
        OB->>DB: Mark Event Delivered
    end
```

---

## 🌍 4. Geo-Distributed Monitoring Architecture

### 4.1 Mode A: AMQP Geo-Worker Pools (Direct AMQP Connection)

```mermaid
sequenceDiagram
    autonumber
    participant S as Central Scheduler
    participant MQ as Central RabbitMQ
    participant GW as Geo-Worker (--region us-east)
    participant Target as Local Intranet Service

    S->>MQ: Publish Job -> Exchange (Routing Key: checks.jobs.us-east)
    MQ->>GW: Deliver Job via AMQP
    GW->>Target: Probe Intranet Target (SSRF Guarded)
    Target-->>GW: Result
    GW->>MQ: Publish Result -> Queue: checks.results
```

### 💡 4.2 Data Flow via RabbitMQ Queues

The diagram below details the exact queue topology and data flow across RabbitMQ in AMQP mode:

```mermaid
flowchart TD
    SCHED["Scheduler (Leader)"]
    
    Q_CORE["Queue: checks.jobs.core"]
    Q_GEO1["Queue: checks.jobs.geo1"]
    
    W_CORE["Worker Pool (Core)"]
    W_GEO1["Worker Pool (Geo-1)"]
    
    Q_RES["Queue: checks.results"]
    
    INGEST["API / Ingest Service"]
    PG[("PostgreSQL")]

    SCHED -->|publish CheckJob| Q_CORE
    SCHED -->|publish CheckJob| Q_GEO1

    Q_CORE -->|consume| W_CORE
    Q_GEO1 -->|consume| W_GEO1

    W_CORE -->|publish Result| Q_RES
    W_GEO1 -->|publish Result| Q_RES

    Q_RES -->|consume| INGEST
    INGEST -->|write heartbeats| PG
```

### 4.3 Mode B: HTTP Pull-Agent (Outbound HTTPS Only)

```mermaid
sequenceDiagram
    autonumber
    participant S as Central Scheduler
    participant DB as PostgreSQL (pull_jobs)
    participant API as Central API (/agent/*)
    participant AG as HTTP Agent (--role agent)
    participant Target as Target Service

    S->>DB: Insert Job into pull_jobs table
    DB-->>API: NOTIFY 'pull_jobs', 'asia-south' (LISTEN/NOTIFY)
    
    Note over AG: Long-Polling Request
    AG->>API: GET /agent/jobs?region=asia-south
    Note over API: Hold Request (Up to 20s or until NOTIFY)
    API->>DB: Claim Job (FOR UPDATE SKIP LOCKED RETURNING)
    API-->>AG: Deliver Jobs Payload
    
    AG->>Target: Execute Probe
    Target-->>AG: Response
    
    alt Network Available (Live Ingestion)
        AG->>API: POST /agent/results?region=asia-south
        Note over API: Region Scoping Check (monitor.region == region)
        API->>DB: Ingest Heartbeat & Reconcile Live Incident
    else Network Interrupted (Edge Buffering)
        Note over AG: Save Result to In-Memory Ring Buffer (cap=10000)
        Note over AG: Network Restored
        AG->>API: POST /agent/backfill?region=asia-south
        API->>DB: InsertHeartbeatsBulk (SLA-only, Bypass Live Reconcile)
        Note over DB: ON CONFLICT DO NOTHING (Historical SLA, No False Alerts)
    end
```

---

## 🔐 5. Authentication & Tenant Authorization (AuthN & AuthZ)

```mermaid
sequenceDiagram
    autonumber
    participant Client as SPA / Client
    participant Auth as Auth Handler
    participant IdP as OIDC Provider (Keycloak/Auth0/Okta)
    participant DB as PostgreSQL
    participant AuthZ as AuthZ Can()

    alt OIDC Login
        Client->>Auth: GET /auth/login
        Auth-->>Client: Redirect to OIDC Issuer (PKCE + State)
        Client->>IdP: Authenticate
        IdP-->>Client: Redirect /auth/callback?code=...
        Client->>Auth: GET /auth/callback
        Auth->>IdP: Exchange Code for Tokens
        Auth->>DB: JIT Provision User & Create Session (Token Hash)
        Auth-->>Client: Set HttpOnly Cookie (session_token)
    else Local Login (Argon2id)
        Client->>Auth: POST /auth/local/login (Username, Password, TOTP)
        Auth->>DB: Verify Argon2id Password & Rate-Limit
        Auth-->>Client: Set HttpOnly Cookie
    end

    Note over Client, AuthZ: Protected API Request
    Client->>Auth: GET /api/v1/projects/{id}/monitors
    Auth->>DB: Resolve Session Cookie -> Principal User
    Auth->>AuthZ: Can(User, ActionProjectRead, ScopeProject)
    
    alt Access Granted
        AuthZ-->>Auth: Allow
        Auth->>DB: Query Monitors WHERE project_id = $1 AND org_id = $2
        DB-->>Client: 200 OK (Data)
    else Access Denied / Cross-Tenant Request
        AuthZ-->>Auth: Deny
        Auth-->>Client: 403 Forbidden / 404 Not Found (Tenant Isolated)
    end
```

---

## 🚨 6. Escalations, On-Call & Incident Handling

```mermaid
sequenceDiagram
    autonumber
    participant Ingest as Ingest Pipeline
    participant DB as PostgreSQL
    participant Sched as Scheduler (AdvanceEscalations)
    participant Outbox as Outbox Worker
    participant User as On-Call Engineer

    Ingest->>DB: Monitor DOWN -> Open Auto-Incident
    Note over DB: Save Incident (started_at = now)

    Note over Sched: Escalation Tick (every 15s)
    Sched->>DB: Load Open Non-Acked Incidents
    Sched->>DB: Evaluate Escalation Policy Step (after_seconds)
    Sched->>DB: Resolve On-Call Schedule for step -> Target User
    Sched->>DB: Enqueue escalation_step Event to Outbox

    Outbox->>User: Send Alert (Telegram/Slack/Email)
    
    Note over User: Engineer receives alert & clicks Acknowledge
    User->>DB: POST /api/v1/incidents/{id}/acknowledge
    Note over DB: Set acknowledged_at = now
    
    Note over Sched: Next Escalation Tick
    Sched->>DB: Incident Acknowledged -> STOP Escalation Ladder!
```

---

## 🗄️ 7. Database Entity-Relationship Diagram & Partitioning

* **Partitioning**: Table `heartbeats` is daily RANGE-partitioned (`RANGE PARTITION BY (ts)`). The leader scheduler automatically creates upcoming partitions and drops old ones according to `retention_days`.
* **Daily Rollup**: Aggregates in `heartbeats_daily` are calculated in the background by the scheduler leader to serve instantaneous 90+ day SLA/SLI reports.

```mermaid
erDiagram
    organizations ||--o{ projects : contains
    projects ||--o{ monitors : contains
    projects ||--o{ incidents : registers
    projects ||--o{ notification_channels : defines
    projects ||--o{ escalation_policies : defines
    projects ||--o{ oncall_schedules : defines

    monitors ||--o{ heartbeats : records
    monitors ||--o{ heartbeats_daily : aggregates

    incidents ||--o{ incident_updates : timeline
    incidents ||--o| postmortems : attaches

    organizations ||--o{ status_pages : owns
    status_pages ||--o{ components : displays

    organizations ||--o{ api_tokens : issues
    organizations ||--o{ webhooks : registers

    heartbeats {
        uuid monitor_id FK
        timestamptz ts PK
        boolean up
        bigint latency_ms
        int code
        text msg
    }

    heartbeats_daily {
        uuid monitor_id PK
        date day PK
        bigint up_count
        bigint total_count
    }

    outbox {
        uuid id PK
        text topic
        jsonb payload
        text status
        int retry_count
        timestamptz next_retry_at
    }
```
