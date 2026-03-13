# OIDC Workload Identity — Architecture Diagrams & Flows

This document contains all architecture diagrams and flow visualizations for the
OIDC workload identity federation setup with Azure Entra across managed OpenShift clusters.

| # | Diagram | What it shows |
|---|---------|---------------|
| 1 | High-Level Component Architecture | Full topology: Azure Entra → DNS → F5 BIG-IP → OpenShift Router → OIDC Proxy → Cluster Config → Workload Pod |
| 2 | Token Validation Flow | Sequence: Entra fetches discovery + JWKS through the reverse proxy chain |
| 3 | Token Exchange Flow | Sequence: App pod requests projected SA token, exchanges with Entra, accesses Azure resources |
| 4 | Key Rotation Timeline | Overlap period requirement when signing keys rotate |
| 5 | Multi-Cluster Overview | Each cluster gets its own issuer, VIP, and Entra registration |
| 6 | ACM + Ansible Hybrid Automation Flow | Label → Placement → PolicySet → PolicyAutomation → AAP → F5/DNS/Entra → ConfigMap feedback loop |
| 7 | Certificate Lifecycle & Monitoring | cert-manager renewal, CertificatePolicy monitoring, Ansible alerting automation |
| 8 | ACM Governance Dashboard View | Compliance matrix with all 8 policies across clusters |
| 9 | End-to-End Onboarding Timeline | Time-based view: T+0s through T+10min after a label is applied |

---

## 1. High-Level Component Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                     AZURE ENTRA ID                                              │
│                                                                                                 │
│  ┌───────────────────────────────┐     ┌──────────────────────────────────────────────────────┐  │
│  │  Federated Identity Credential│     │  Token Validation Flow                               │  │
│  │                               │     │                                                      │  │
│  │  Issuer (must match exactly): │     │  1. Workload presents SA token to Entra              │  │
│  │  https://openshift-oidc-      │     │  2. Entra reads "iss" claim from JWT                 │  │
│  │  <clustername>.auth.          │     │  3. Entra fetches /.well-known/openid-configuration  │  │
│  │  mydomain.no                  │     │     from issuer URL                                  │  │
│  │                               │     │  4. Entra fetches JWKS from jwks_uri in discovery    │  │
│  │  Subject: system:serviceacc.. │     │  5. Entra validates JWT signature against JWKS       │  │
│  │  Audience: api://AzureAD...   │     │  6. If valid → issues Azure access token             │  │
│  └───────────────────────────────┘     └───────────────────┬──────────────────────────────────┘  │
│                                                            │                                     │
└────────────────────────────────────────────────────────────┼────────────────────────────────────-┘
                                                             │
                            Steps 3 & 4: HTTPS GET           │
                            (outbound from Azure)            │
                                                             │
                ┌────────────────────────────────────────────┼──────────────────────────┐
                │                        DNS                 │                          │
                │  openshift-oidc-<clustername>.auth.mydomain.no                        │
                │  ──► resolves to F5 BIG-IP VIP             │                          │
                └────────────────────────────────────────────┼──────────────────────────┘
                                                             │
                                                             ▼
┌───────────────────────────────────────────────────────────────────────────────────────────  ─────┐
│                                    F5 BIG-IP                                                     │
│                                                                                                  │
│  Virtual Server: openshift-oidc-<clustername>.auth.mydomain.no:443                               │
│                                                                                                  │
│  ┌──────────────────────────────────────────────────────────────────────────────────────── ──┐   │
│  │  TLS HANDLING OPTIONS (choose one):                                                       │   │
│  │                                                                                           │   │
│  │  Option A: TLS Passthrough                          Option B: Re-encrypt                  │   │
│  │  ┌─────────────────────────────────┐                ┌──────────────────────────────────┐  │   │
│  │  │ • BIG-IP does NOT terminate TLS │                │ • BIG-IP terminates TLS          │  │   │
│  │  │ • Traffic passes encrypted to   │                │ • Inspects/logs if needed        │  │   │
│  │  │   OpenShift Router              │                │ • Re-encrypts to OpenShift       │  │   │
│  │  │ • Simpler config                │                │   Router with internal cert      │  │   │
│  │  │ • No cert needed on BIG-IP      │                │ • Needs valid cert on BIG-IP     │  │   │
│  │  │ • Cannot inspect traffic        │                │ • More complex, more control     │  │   │
│  │  └─────────────────────────────────┘                └──────────────────────────────────┘  │   │
│  └───────────────────────────────────────────────────────────────────────────────────────── ─┘   │
│                                                                                                  │
│  Pool Member: <OpenShift Router IP>:443                                                          │
│  Health Monitor: GET /.well-known/openid-configuration expecting HTTP 200                        │
│                                                                                                  │
│  ┌──────────────────────────────────────────────────────────────────────────────────────────┐    │
│  │  Configured by: Ansible (f5networks.f5_modules collection)                               │    │
│  │  Triggered by:  ACM PolicyAutomation → AAP Controller                                    │    │
│  └──────────────────────────────────────────────────────────────────────────────────────────┘    │
│                                                                                                  │
└───────────────────────────────────────────┬────────────────────────────────────────────────────  ┘
                                            │
                                            │  HTTPS (encrypted)
                                            │
                                            ▼
┌────────────────────────────────────────────────────────────────────────────────────────────────┐
│                              OPENSHIFT CLUSTER (Baremetal)                                      │
│                                                                                                │
│  ┌─────────────────────────────────────────────────────────────────────────────────────────┐    │
│  │                         OPENSHIFT ROUTER (HAProxy)                                      │    │
│  │                                                                                         │    │
│  │  Route: openshift-oidc-<clustername>.auth.mydomain.no                                   │    │
│  │                                                                                         │    │
│  │  ┌───────────────────────────────────────────────────────────────────────────────────┐   │    │
│  │  │  TLS Termination: Edge                                                            │   │    │
│  │  │  Certificate: Managed by cert-manager (auto-renewed)                               │   │    │
│  │  │               Monitored by ACM CertificatePolicy (30-day alert)                    │   │    │
│  │  │                                                                                    │   │    │
│  │  │  NOTE: If BIG-IP uses passthrough, this Route handles ALL TLS termination.         │   │    │
│  │  │        Cert here MUST be trusted by Azure (public CA or configured trust).         │   │    │
│  │  │        If BIG-IP re-encrypts, this can be an internal/self-signed cert.            │   │    │
│  │  └───────────────────────────────────────────────────────────────────────────────────┘   │    │
│  │                                                                                         │    │
│  │  Backend: oidc-proxy-service.openshift-oidc-proxy.svc.cluster.local:8080                │    │
│  │                                                                                         │    │
│  └──────────────────────────────────────────┬──────────────────────────────────────────────┘    │
│                                             │                                                  │
│                                             │  HTTP (unencrypted, cluster-internal)             │
│                                             ▼                                                  │
│  ┌──────────────────────────────────────────────────────────────────────────────────────────┐   │
│  │                       OIDC PROXY SERVICE (Deployment)                                    │   │
│  │                       Namespace: openshift-oidc-proxy                                    │   │
│  │                                                                                          │   │
│  │  Replicas: 2+ (HA, with pod anti-affinity)                                              │   │
│  │  Deployed by: ACM Policy (oidc-proxy-deployment)                                        │   │
│  │                                                                                          │   │
│  │  Exposed Endpoints:                                                                      │   │
│  │  ┌────────────────────────────────────────────────────────────────────────────────────┐   │   │
│  │  │                                                                                    │   │   │
│  │  │  GET /.well-known/openid-configuration                                             │   │   │
│  │  │  Returns:                                                                          │   │   │
│  │  │  {                                                                                 │   │   │
│  │  │    "issuer": "https://openshift-oidc-<clustername>.auth.mydomain.no",              │   │   │
│  │  │    "jwks_uri": "https://openshift-oidc-<clustername>.auth.mydomain.no/jwks",       │   │   │
│  │  │    "response_types_supported": ["id_token"],                                       │   │   │
│  │  │    "subject_types_supported": ["public"],                                          │   │   │
│  │  │    "id_token_signing_alg_values_supported": ["RS256"]                              │   │   │
│  │  │  }                                                                                 │   │   │
│  │  │                                                                                    │   │   │
│  │  │  GET /jwks                                                                         │   │   │
│  │  │  Returns: { "keys": [ { "kty":"RSA", "kid":"...", "n":"...", "e":"..." } ] }       │   │   │
│  │  │                                                                                    │   │   │
│  │  │  Cache-Control: max-age=3600 (serve cached, refresh periodically)                  │   │   │
│  │  │                                                                                    │   │   │
│  │  └────────────────────────────────────────────────────────────────────────────────────┘   │   │
│  │                                                                                          │   │
│  │  ┌────────────────────────────────────────────────────────────────────────────────────┐   │   │
│  │  │  INTERNAL: Fetches data from API Server                                            │   │   │
│  │  │                                                                                    │   │   │
│  │  │  ServiceAccount: oidc-proxy-sa                                                     │   │   │
│  │  │  Permissions: MINIMAL — ClusterRole oidc-proxy-reader                              │   │   │
│  │  │    - nonResourceURLs: [/openid/v1/jwks] verbs: [get]                               │   │   │
│  │  │    - nonResourceURLs: [/.well-known/*]  verbs: [get]                               │   │   │
│  │  │                                                                                    │   │   │
│  │  │  Reads from:                                                                       │   │   │
│  │  │    https://kubernetes.default.svc/openid/v1/jwks                                   │   │   │
│  │  │    https://kubernetes.default.svc/.well-known/oauth-authorization-server            │   │   │
│  │  │                                                                                    │   │   │
│  │  │  Refresh interval: every 5-10 min (picks up key rotations)                         │   │   │
│  │  │  Serves cached response on every request (no real-time API calls)                  │   │   │
│  │  └────────────────────────────────────────────────────────────────────────────────────┘   │   │
│  └──────────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                                │
│  ┌──────────────────────────────────────────────────────────────────────────────────────────┐   │
│  │                     CLUSTER CONFIGURATION (Deployed by ACM Policy)                       │   │
│  │                                                                                          │   │
│  │  apiVersion: config.openshift.io/v1                                                      │   │
│  │  kind: Authentication                                                                    │   │
│  │  metadata:                                                                               │   │
│  │    name: cluster                                                                         │   │
│  │  spec:                                                                                   │   │
│  │    serviceAccountIssuer: "https://openshift-oidc-<clustername>.auth.mydomain.no"         │   │
│  │                                                                                          │   │
│  │  ⚠ CAUTION: Changing this invalidates existing SA tokens.                                │   │
│  │             Plan for transition period. API server pods will restart.                     │   │
│  │                                                                                          │   │
│  └──────────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                                │
│  ┌──────────────────────────────────────────────────────────────────────────────────────────┐   │
│  │                     WORKLOAD (Application Pod)                                           │   │
│  │                                                                                          │   │
│  │  1. Pod mounts projected SA token volume:                                                │   │
│  │     - audience: "api://AzureADTokenExchange" (or custom)                                 │   │
│  │     - expirationSeconds: 3600                                                            │   │
│  │                                                                                          │   │
│  │  2. Token contains:                                                                      │   │
│  │     {                                                                                    │   │
│  │       "iss": "https://openshift-oidc-<clustername>.auth.mydomain.no",  ◄── must match    │   │
│  │       "sub": "system:serviceaccount:<ns>:<sa-name>",                                     │   │
│  │       "aud": ["api://AzureADTokenExchange"],                                             │   │
│  │       "exp": 1234567890                                                                  │   │
│  │     }                                                                                    │   │
│  │                                                                                          │   │
│  │  3. App exchanges this token with Entra for Azure access token                           │   │
│  │     POST https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token                    │   │
│  │       grant_type=client_credentials                                                      │   │
│  │       client_id=<app-registration-client-id>                                             │   │
│  │       client_assertion=<SA-JWT-token>                                                    │   │
│  │       client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer       │   │
│  │       scope=https://graph.microsoft.com/.default (or other)                              │   │
│  │                                                                                          │   │
│  └──────────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                                │
└────────────────────────────────────────────────────────────────────────────────────────────────-┘
```

---

## 2. Token Validation Flow (Entra → OIDC Proxy)

When Azure Entra needs to validate a service account JWT token, it fetches the OIDC discovery
document and JWKS keys from the issuer URL. This is the path that request takes:

```
  Azure Entra                   F5 BIG-IP              OCP Router            OIDC Proxy
      │                             │                      │                      │
      │  HTTPS GET                  │                      │                      │
      │  /.well-known/openid─config │                      │                      │
      │────────────────────────────►│                      │                      │
      │                             │  TLS passthrough     │                      │
      │                             │  or re-encrypt       │                      │
      │                             │─────────────────────►│                      │
      │                             │                      │  TLS terminated      │
      │                             │                      │  HTTP to backend     │
      │                             │                      │─────────────────────►│
      │                             │                      │                      │ Return cached
      │                             │                      │                      │ discovery JSON
      │                             │                      │◄─────────────────────│
      │                             │◄─────────────────────│                      │
      │◄────────────────────────────│                      │                      │
      │                             │                      │                      │
      │  HTTPS GET /jwks            │                      │                      │
      │────────────────────────────►│─────────────────────►│─────────────────────►│
      │                             │                      │                      │ Return cached
      │                             │                      │                      │ JWKS JSON
      │◄────────────────────────────│◄─────────────────────│◄─────────────────────│
      │                             │                      │                      │
      │  Validate JWT signature     │                      │                      │
      │  against JWKS keys          │                      │                      │
      │  ✓ Valid → issue Azure token│                      │                      │
      │                             │                      │                      │
```

---

## 3. Token Exchange Flow (Application Pod → Azure)

When an application on the cluster needs to access Azure resources, it exchanges its
OpenShift service account token for an Azure access token:

```
  App Pod              OCP API Server           Azure Entra            Azure Resource
     │                       │                       │                       │
     │ Request projected     │                       │                       │
     │ SA token (audience:   │                       │                       │
     │ api://AzureAD...)     │                       │                       │
     │──────────────────────►│                       │                       │
     │◄──────────────────────│                       │                       │
     │  JWT with iss=        │                       │                       │
     │  openshift-oidc-...   │                       │                       │
     │                       │                       │                       │
     │  POST /oauth2/v2.0/token                      │                       │
     │  (client_assertion=JWT)                       │                       │
     │──────────────────────────────────────────────►│                       │
     │                       │                       │                       │
     │                       │    (Entra fetches      │                       │
     │                       │     OIDC discovery     │                       │
     │                       │     + JWKS if not      │                       │
     │                       │     cached — see       │                       │
     │                       │     flow 2 above)      │                       │
     │                       │                       │                       │
     │  Azure access token   │                       │                       │
     │◄──────────────────────────────────────────────│                       │
     │                       │                       │                       │
     │  Bearer <azure-token> │                       │                       │
     │──────────────────────────────────────────────────────────────────────►│
     │                       │                       │              ✓ Access │
     │◄──────────────────────────────────────────────────────────────────────│
     │                       │                       │                       │
```

---

## 4. Key Rotation Timeline

OpenShift periodically rotates service account signing keys. The OIDC proxy must serve
both old and new keys during the transition period. Entra caches JWKS for ~24 hours.

```
  Time ──────────────────────────────────────────────────────────────────────────►

  │  Key A active        │  Key A + Key B both in JWKS  │  Key B only          │
  │  JWKS: [A]           │  JWKS: [A, B]                │  JWKS: [B]           │
  │                      │                              │                      │
  │                      │◄──── OVERLAP PERIOD ────────►│                      │
  │                      │  Must be > Entra JWKS cache  │                      │
  │                      │  lifetime (~24 hrs)          │                      │
  │                      │                              │                      │
  │                      │  Proxy must serve BOTH keys  │                      │
  │                      │  during this window          │                      │
```

---

## 5. Multi-Cluster Overview

Each cluster gets its own issuer URL, OIDC proxy, and corresponding F5/DNS/Entra config.
All managed centrally from the ACM hub.

```
                              ┌─────────────────┐
                              │   Azure Entra    │
                              │                  │
                              │  Fed. Cred #1 ─────► issuer: openshift-oidc-cluster1.auth...
                              │  Fed. Cred #2 ─────► issuer: openshift-oidc-cluster2.auth...
                              │  Fed. Cred #3 ─────► issuer: openshift-oidc-cluster3.auth...
                              │                  │
                              └───────┬──────────┘
                                      │
                        ┌─────────────┼─────────────┐
                        │             │             │
                        ▼             ▼             ▼
                  ┌──────────┐ ┌──────────┐ ┌──────────┐
                  │ BIG-IP   │ │ BIG-IP   │ │ BIG-IP   │
                  │ VIP #1   │ │ VIP #2   │ │ VIP #3   │
                  └────┬─────┘ └────┬─────┘ └────┬─────┘
                       │            │            │
                       ▼            ▼            ▼
                  ┌──────────┐ ┌──────────┐ ┌──────────┐
                  │ OCP      │ │ OCP      │ │ OCP      │
                  │ Cluster1 │ │ Cluster2 │ │ Cluster3 │
                  │ (bmetal) │ │ (bmetal) │ │ (bmetal) │
                  │          │ │          │ │          │
                  │ Router   │ │ Router   │ │ Router   │
                  │   ↓      │ │   ↓      │ │   ↓      │
                  │ OIDC     │ │ OIDC     │ │ OIDC     │
                  │ Proxy    │ │ Proxy    │ │ Proxy    │
                  └──────────┘ └──────────┘ └──────────┘

  Each cluster: identical setup, different issuer name, own cert, own Entra registration
  Managed via ACM PolicySet + Ansible Automation Platform
```

---

## 6. ACM + Ansible Hybrid Automation Flow

This diagram shows how adding a single label to a cluster triggers the entire
onboarding pipeline — both in-cluster (ACM) and external (Ansible).

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                         ACM HUB CLUSTER                                         │
│                                                                                 │
│  ┌──────────────────────┐                                                       │
│  │  Admin adds label:   │                                                       │
│  │  oidc-federation     │                                                       │
│  │  =enabled            │                                                       │
│  └──────────┬───────────┘                                                       │
│             │                                                                   │
│             ▼                                                                   │
│  ┌──────────────────────┐         ┌──────────────────────────────────────────┐   │
│  │  Placement evaluates │────────►│  PolicySet: oidc-workload-identity       │   │
│  │  cluster matches     │         │                                          │   │
│  └──────────────────────┘         │  Policies 1-7: enforce (in-cluster)      │   │
│                                   │    • Namespace                            │   │
│                                   │    • ServiceAccount + RBAC               │   │
│                                   │    • OIDC Proxy Deployment + Service     │   │
│                                   │    • Route (TLS edge)                    │   │
│                                   │    • Authentication CR (issuer)          │   │
│                                   │    • cert-manager Certificate            │   │
│                                   │    • CertificatePolicy (monitoring)      │   │
│                                   │                                          │   │
│                                   │  Policy 8: inform (canary check)         │   │
│                                   │    • ConfigMap oidc-external-infra-ready │   │
│                                   │    • NonCompliant → triggers automation  │   │
│                                   └──────────────────┬───────────────────────┘   │
│                                                      │                          │
│                                          Policy 8 = NonCompliant                │
│                                          (ConfigMap doesn't exist yet)          │
│                                                      │                          │
│                                                      ▼                          │
│  ┌──────────────────────────────────────────────────────────────────────────┐    │
│  │  PolicyAutomation: oidc-external-infra-automation                       │    │
│  │  mode: everyEvent                                                       │    │
│  │  policyRef: oidc-external-infra-check                                   │    │
│  │                                                                          │    │
│  │  Creates AnsibleJob CR with:                                             │    │
│  │    target_clusters: ["<new-cluster-name>"]                               │    │
│  │    extra_vars: { dns_zone, azure_tenant_id, ... }                        │    │
│  └───────────────────────────────────┬──────────────────────────────────────┘    │
│                                      │                                          │
│  ┌───────────────────────────────────┼──────────────────────────────────────┐    │
│  │  AAP Resource Operator            │                                      │    │
│  │  (watches AnsibleJob CRs)         │                                      │    │
│  │  Picks up the job, sends to ──────┼──────────────────────┐               │    │
│  │  AAP Controller via API token     │                      │               │    │
│  └───────────────────────────────────┘                      │               │    │
│                                                             │               │    │
└─────────────────────────────────────────────────────────────┼───────────────────-┘
                                                              │
                                                              ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    ANSIBLE AUTOMATION PLATFORM CONTROLLER                        │
│                                                                                 │
│  Job Template: OIDC-External-Infra-Onboarding                                   │
│  Receives: target_clusters = ["<new-cluster-name>"]                             │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐     │
│  │                                                                         │     │
│  │  Step 1: Configure F5 BIG-IP                                            │     │
│  │  ┌───────────────────────────────────────────────────────────────────┐   │     │
│  │  │  • Create pool: pool_oidc_<clustername>                           │   │     │
│  │  │  • Add pool member: <cluster-router-ip>:443                       │   │     │
│  │  │  • Create HTTPS health monitor (checks /.well-known/openid-conf)  │   │     │
│  │  │  • Create virtual server: vs_oidc_<clustername>                   │   │     │
│  │  │  • Configure TLS profile (passthrough or re-encrypt)              │   │     │
│  │  │  Uses: f5networks.f5_modules Ansible collection                   │   │     │
│  │  └───────────────────────────────────────────────────────────────────┘   │     │
│  │                          │                                              │     │
│  │                          ▼                                              │     │
│  │  Step 2: Create DNS Record                                              │     │
│  │  ┌───────────────────────────────────────────────────────────────────┐   │     │
│  │  │  • A record: openshift-oidc-<clustername>.auth.mydomain.no       │   │     │
│  │  │  • Points to: F5 VIP address                                     │   │     │
│  │  └───────────────────────────────────────────────────────────────────┘   │     │
│  │                          │                                              │     │
│  │                          ▼                                              │     │
│  │  Step 3: Register Azure Entra Federated Identity Credential             │     │
│  │  ┌───────────────────────────────────────────────────────────────────┐   │     │
│  │  │  • Issuer: https://openshift-oidc-<clustername>.auth.mydomain.no │   │     │
│  │  │  • Subject: system:serviceaccount:<ns>:<sa>                      │   │     │
│  │  │  • Audience: api://AzureADTokenExchange                          │   │     │
│  │  │  Uses: azure.azcollection Ansible collection                     │   │     │
│  │  └───────────────────────────────────────────────────────────────────┘   │     │
│  │                          │                                              │     │
│  │                          ▼                                              │     │
│  │  Step 4: Create ConfigMap on Managed Cluster (feedback signal)          │     │
│  │  ┌───────────────────────────────────────────────────────────────────┐   │     │
│  │  │  ConfigMap: oidc-external-infra-ready                             │   │     │
│  │  │  Namespace: openshift-oidc-proxy                                  │   │     │
│  │  │  Data: f5_configured=true, dns_configured=true,                   │   │     │
│  │  │        entra_configured=true, configured_at=<timestamp>           │   │     │
│  │  │  Uses: kubernetes.core Ansible collection (via ACM proxy or       │   │     │
│  │  │        direct kubeconfig)                                         │   │     │
│  │  └───────────────────────────────────────────────────────────────────┘   │     │
│  │                                                                         │     │
│  └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      │ ConfigMap created on managed cluster
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│                         MANAGED CLUSTER                                         │
│                                                                                 │
│  ConfigMap oidc-external-infra-ready now exists                                 │
│  → ACM Policy 8 becomes ✅ Compliant                                            │
│  → PolicyAutomation stops triggering                                            │
│  → All 8 policies green in Governance dashboard                                 │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 7. Certificate Lifecycle & Monitoring Flow

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    CERTIFICATE LIFECYCLE                                         │
│                                                                                 │
│  ┌─────────────────┐     ┌──────────────────┐      ┌────────────────────────┐   │
│  │  ACM Policy 6   │     │  cert-manager    │      │  cert-manager          │   │
│  │  deploys        │────►│  Certificate CR  │─────►│  requests cert from    │   │
│  │  Certificate CR │     │  on cluster      │      │  ClusterIssuer         │   │
│  └─────────────────┘     └──────────────────┘      │  (ACME/internal CA)    │   │
│                                                     └───────────┬────────────┘   │
│                                                                 │               │
│                                                       Cert issued &             │
│                                                       stored in Secret          │
│                                                       oidc-proxy-tls            │
│                                                                 │               │
│                                                                 ▼               │
│  ┌──────────────────────────────────────────────────────────────────────────┐    │
│  │  MONITORING (two independent checks)                                    │    │
│  │                                                                          │    │
│  │  Check 1: CertificatePolicy (ACM Policy 7)                              │    │
│  │  ┌───────────────────────────────────────────────────────────────────┐   │    │
│  │  │  Scans: TLS Secrets in openshift-oidc-proxy namespace             │   │    │
│  │  │  Alert: When cert expires within 720h (30 days)                   │   │    │
│  │  │  Also checks: maximumDuration, disallowedSANPattern              │   │    │
│  │  │  Action: Reports NonCompliant to ACM hub                          │   │    │
│  │  └───────────────────────────────────────┬───────────────────────────┘   │    │
│  │                                          │                              │    │
│  │  Check 2: ConfigurationPolicy (cert-manager status)                     │    │
│  │  ┌───────────────────────────────────────────────────────────────────┐   │    │
│  │  │  Verifies: Certificate CR status.conditions.Ready = "True"        │   │    │
│  │  │  Catches: cert-manager failures (DNS challenge, issuer error)     │   │    │
│  │  │  Action: Reports NonCompliant to ACM hub                          │   │    │
│  │  └───────────────────────────────────────┬───────────────────────────┘   │    │
│  │                                          │                              │    │
│  └──────────────────────────────────────────┼──────────────────────────────┘    │
│                                             │                                   │
│                              If NonCompliant │                                   │
│                                             ▼                                   │
│  ┌──────────────────────────────────────────────────────────────────────────┐    │
│  │  PolicyAutomation: oidc-cert-expiry-automation                          │    │
│  │  mode: everyEvent (max once per 24h)                                    │    │
│  │                                                                          │    │
│  │  Triggers Ansible job: OIDC-Certificate-Expiry-Alert                    │    │
│  │  ┌───────────────────────────────────────────────────────────────────┐   │    │
│  │  │  • Send Slack/Teams notification to #platform-alerts              │   │    │
│  │  │  • Open ServiceNow incident (severity: high)                     │   │    │
│  │  │  • Optionally: force cert-manager renewal via annotation          │   │    │
│  │  │  • Optionally: update F5 TLS profile with new cert               │   │    │
│  │  └───────────────────────────────────────────────────────────────────┘   │    │
│  └──────────────────────────────────────────────────────────────────────────┘    │
│                                                                                 │
│                                                                                 │
│  RENEWAL (handled by cert-manager automatically):                               │
│                                                                                 │
│  Time ──────────────────────────────────────────────────────────────────────►    │
│                                                                                 │
│  │ Certificate    │ renewBefore: 720h  │ cert-manager  │ New cert     │         │
│  │ issued         │ (30 days before    │ auto-renews   │ active       │         │
│  │ duration: 90d  │  expiry)           │               │              │         │
│  │                │                    │               │              │         │
│  │◄── 60 days ───►│◄── renew window ─►│               │              │         │
│  │                │                    │               │              │         │
│  │                │  If renewal fails: │               │              │         │
│  │                │  CertificatePolicy │               │              │         │
│  │                │  alerts at 30d     │               │              │         │
│  │                │  before expiry     │               │              │         │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 8. ACM Governance — Policy Dependency & Compliance View

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    ACM GOVERNANCE DASHBOARD                                      │
│                                                                                 │
│  PolicySet: oidc-workload-identity                                              │
│                                                                                 │
│  ┌────────────────────────────────────────────────────────────────────────────┐  │
│  │  Policy                         │ Type      │ Remediation │ Depends On    │  │
│  ├────────────────────────────────────────────────────────────────────────────┤  │
│  │  1. oidc-proxy-namespace        │ Config    │ enforce     │ (none)        │  │
│  │  2. oidc-proxy-rbac             │ Config    │ enforce     │ Policy 1      │  │
│  │  3. oidc-proxy-deployment       │ Config    │ enforce     │ Policy 1, 2   │  │
│  │  4. oidc-proxy-route            │ Config    │ enforce     │ Policy 1, 3   │  │
│  │  5. oidc-issuer-config          │ Config    │ enforce     │ (none)        │  │
│  │  6. oidc-cert-manager           │ Config    │ enforce     │ Policy 1      │  │
│  │  7. oidc-certificate-monitoring │ Cert      │ inform      │ Policy 6      │  │
│  │  8. oidc-external-infra-check   │ Config    │ inform      │ Policy 1-6    │  │
│  └────────────────────────────────────────────────────────────────────────────┘  │
│                                                                                 │
│  PolicyAutomation resources (not visible in PolicySet, but linked):             │
│  ┌────────────────────────────────────────────────────────────────────────────┐  │
│  │  oidc-external-infra-automation │ Watches Policy 8 │ Triggers Ansible     │  │
│  │  oidc-cert-expiry-automation    │ Watches Policy 7 │ Triggers Ansible     │  │
│  └────────────────────────────────────────────────────────────────────────────┘  │
│                                                                                 │
│  Compliance Matrix:                                                             │
│  ┌──────────────────────────┬──────────┬──────────┬──────────┐                  │
│  │ Policy                   │ Cluster1 │ Cluster2 │ Cluster3 │                  │
│  ├──────────────────────────┼──────────┼──────────┼──────────┤                  │
│  │ oidc-proxy-namespace     │ ✅       │ ✅       │ ✅       │                  │
│  │ oidc-proxy-rbac          │ ✅       │ ✅       │ ✅       │                  │
│  │ oidc-proxy-deployment    │ ✅       │ ✅       │ ✅       │                  │
│  │ oidc-proxy-route         │ ✅       │ ✅       │ ✅       │                  │
│  │ oidc-issuer-config       │ ✅       │ ✅       │ ✅       │                  │
│  │ oidc-cert-manager        │ ✅       │ ✅       │ ✅       │                  │
│  │ oidc-cert-monitoring     │ ✅       │ ✅       │ ⚠️ 25d   │                  │
│  │ oidc-external-infra      │ ✅       │ ✅       │ ✅       │                  │
│  └──────────────────────────┴──────────┴──────────┴──────────┘                  │
│                                                                                 │
│  Cluster3 alert: Certificate in openshift-oidc-proxy/oidc-proxy-tls             │
│  expires in 25 days — PolicyAutomation has triggered Ansible alert playbook      │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 9. End-to-End Onboarding Timeline

```
  Time ───────────────────────────────────────────────────────────────────────────────────────►

  T+0s         T+5s              T+30s              T+2min             T+5min        T+10min
    │            │                  │                  │                  │              │
    │  Admin     │  ACM Placement   │  Policies        │  Policy 8        │  Ansible     │
    │  adds      │  evaluates,      │  1-7 start       │  NonCompliant    │  job         │
    │  label     │  policies        │  deploying       │  triggers        │  completes   │
    │            │  propagated      │  in-cluster      │  PolicyAutomation│              │
    │            │  to cluster      │  resources       │                  │  ConfigMap   │
    │            │                  │                  │  AnsibleJob CR   │  created     │
    │            │                  │                  │  created on hub  │              │
    │            │                  │                  │                  │  F5 ✅       │
    │            │                  │  Namespace ✅    │  AAP picks up    │  DNS ✅      │
    │            │                  │  RBAC ✅         │  job, runs       │  Entra ✅    │
    │            │                  │  Deployment ✅   │  workflow         │              │
    │            │                  │  Route ✅        │                  │  Policy 8    │
    │            │                  │  Issuer ✅       │                  │  → Compliant │
    │            │                  │  CertMgr ✅      │                  │              │
    │            │                  │                  │                  │  All 8       │
    │            │                  │                  │                  │  policies ✅ │
    │            │                  │                  │                  │              │
    ▼            ▼                  ▼                  ▼                  ▼              ▼

  Single       Automatic          In-cluster          External infra     Fully
  label        propagation        ready               automation         operational
                                                      triggered
```
