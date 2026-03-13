# Complete Deployment Guide for OpenShift

This repository implements a **three-layer workload identity federation framework** using ACM (Advanced Cluster Management). Here's the full deployment walkthrough.

---

## Prerequisites

Before deploying anything, you need:

1. **ACM Hub cluster** with ACM 2.14+ installed
2. **Managed clusters** registered with ACM
3. **`oc` CLI** authenticated to the hub cluster
4. **PolicyGenerator plugin** for Kustomize (installed on the hub or via ArgoCD with `--enable-alpha-plugins`)
5. **A `ManagedClusterSet`** named `all-openshift-clusters` containing your clusters
6. **A namespace** `acm-policies` on the hub for policy objects

```bash
oc new-project acm-policies
```

---

## Phase 0 — Customization (BEFORE deploying)

Several placeholder values must be replaced with your real infrastructure details:

| File | What to change |
|------|----------------|
| `acm_cert_manager/manifests/ca-clusterissuer/ca-clusterissuer-secret.yml` | Replace placeholder CA cert+key with real ones |
| `acm_cert_manager/manifests/operatorpolicy.yml` | Verify `stable-v1.17` channel matches your OCP version |
| `acm_external_policies/manifests/05-deployment.yaml` | Set real OIDC proxy image URL (`registry.mydomain.no`) |
| `acm_external_policies/manifests/07-route.yaml` | Set your base domain in hub template |
| `acm_external_policies/manifests/09-cert-manager-certificate.yaml` | ClusterIssuer name (e.g., `letsencrypt-prod`) |
| `acm_external_policies/secrets/aap-credentials-secret.yaml.template` | AAP token, host, Azure tenant/app IDs |
| `acm_external_policies/ansible/group_vars/all.yml` | F5, DNS provider, Azure Entra, cluster access details |
| `acm_internal_policies/manifests/operator-side/08-operator-deployment.yaml` | Operator container image and service URL |
| `acm_internal_policies/manifests/service-side/03-deployment.yaml` | Integration service container image |
| `argocd/applications/*.yaml` | Git repo URL in all ArgoCD Applications |
| `argocd/app-of-apps.yaml` | Git repo URL for the parent Application |
| `argocd/applicationset.yaml` | Git repo URL for the ApplicationSet |

---

## Phase 1 — Shared Resources & cert-manager (Foundation)

### Step 1: Deploy shared placements and cluster set binding

```bash
# ManagedClusterSetBinding — allows Placements in acm-policies to reference the cluster set
oc apply -f shared/managed-cluster-set-binding.yaml -n acm-policies

# All 5 Placements (hub, oidc-federation, oidc-issuer-change, internal-trust-operator, internal-trust-service)
oc apply -f shared/placements/ -n acm-policies
```

**What this creates:**
- 1 `ManagedClusterSetBinding` — binds `all-openshift-clusters` to the `acm-policies` namespace
- 5 `Placement` objects — each selects clusters by label (`oidc-federation=enabled`, `internal-trust=operator`, etc.)

No clusters are matched yet — labels haven't been applied.

### Step 2: Deploy cert-manager policies

```bash
cd acm_cert_manager
kustomize build --enable-alpha-plugins . | oc apply -f - -n acm-policies
```

**What this creates (via PolicyGenerator):**
- ACM Policy `cert-manager-operator` — installs cert-manager operator via OLM (`OperatorPolicy`, enforce mode)
- ACM Policy `cert-manager-clusterissuer` — creates the CA `Secret` + `ClusterIssuer` (depends on operator being Compliant)
- ACM Policy `cert-manager-status` — monitors all cert-manager Deployments are ready (inform only)
- ConfigMap `trusted-ca` — injects cluster CA bundle into cert-manager namespace

These policies target clusters with label `oidc-federation=enabled`.

---

## Phase 2 — OIDC External Trust (Azure Entra Federation)

### Step 3: Create AAP credentials secret

```bash
# Copy the template and fill in real values
cp acm_external_policies/secrets/aap-credentials-secret.yaml.template \
   acm_external_policies/secrets/aap-credentials-secret.yaml

# Edit with your AAP token, host, Azure tenant ID, and app ID
vi acm_external_policies/secrets/aap-credentials-secret.yaml

# Apply the secret to the hub
oc apply -f acm_external_policies/secrets/aap-credentials-secret.yaml -n acm-policies
```

### Step 4: Deploy external trust policies

```bash
cd acm_external_policies
kustomize build --enable-alpha-plugins . | oc apply -f - -n acm-policies

# Also apply the PolicyAutomations
oc apply -f policy-automation.yaml -n acm-policies
oc apply -f policy-automation-cert-expiry.yaml -n acm-policies
```

**What this creates (8 policies in 2 PolicySets):**

| Policy | Resources | Mode |
|--------|-----------|------|
| 1 - namespace | `Namespace` openshift-oidc-proxy | enforce |
| 2 - rbac | `ServiceAccount`, `ClusterRole`, `ClusterRoleBinding` for OIDC proxy | enforce |
| 3 - deployment | `Deployment` (2 replicas, HA) + `Service` for OIDC proxy | enforce |
| 4 - route | OpenShift `Route` with TLS edge termination + cert-manager annotations | enforce |
| 5 - authentication | `Authentication` CR — changes `serviceAccountIssuer` (**destructive, gated**) | enforce |
| 6 - certificate | cert-manager `Certificate` for OIDC proxy TLS (90d duration, 30d renewal) | enforce |
| 7 - cert-monitoring | `CertificatePolicy` — alerts when certs approach expiry | inform |
| 8 - infra-check | Checks for `oidc-external-infra-ready` ConfigMap (canary) | inform |

Plus 2 `PolicyAutomation` objects that trigger Ansible when policies 7 or 8 go NonCompliant.

### Step 5: Label clusters to start deployment

```bash
# Enable OIDC federation on target clusters
oc label managedcluster <cluster-name> oidc-federation=enabled vendor=OpenShift --overwrite
```

This activates policies 1-4, 6-7 immediately. Policy 5 (Authentication CR) is gated — it requires an additional label.

### Step 6: Configure AAP (Ansible Automation Platform)

On your AAP controller:
1. Create Job Template `OIDC-External-Infra-Onboarding` pointing to `acm_external_policies/ansible/playbooks/oidc-full-onboarding.yml`
2. Create Job Template `OIDC-Certificate-Expiry-Alert` for cert expiry notifications
3. Ensure AAP can reach: F5 BIG-IP, DNS provider (Infoblox/Azure DNS/nsupdate), managed cluster APIs

The Ansible automation handles:
- **F5 BIG-IP**: Creates virtual server, pool, SSL profile, health monitor
- **DNS**: Creates A record (auto-selects provider from config)
- **Azure Entra**: Registers federated identity credential on managed identity
- **Feedback**: Creates `oidc-external-infra-ready` ConfigMap on the cluster

### Step 7: Approve the issuer change (when ready)

> **WARNING**: This changes the Kubernetes API server `serviceAccountIssuer`, which **restarts the API server** and **invalidates all existing service account tokens**.

```bash
# Only when you're ready for the API server restart:
oc label managedcluster <cluster-name> oidc-issuer-change=approved --overwrite
```

### Step 8: Build and push the OIDC proxy image

The reference implementation is in `reference-images/oidc-proxy/`:

```bash
cd reference-images/oidc-proxy
podman build -t registry.mydomain.no/platform/oidc-proxy:latest .
podman push registry.mydomain.no/platform/oidc-proxy:latest
```

This is a Go proxy that fetches `/.well-known/openid-configuration` and `/openid/v1/jwks` from the local Kubernetes API and rewrites the issuer URL to the external OIDC endpoint.

---

## Phase 3 — Internal Cross-Cluster Trust (PSAT/TokenReview)

### Step 9: Deploy internal trust policies

```bash
cd acm_internal_policies
kustomize build --enable-alpha-plugins . | oc apply -f - -n acm-policies
```

**What this creates (9 policies in 3 PolicySets):**

**Operator clusters** (label: `internal-trust=operator`):

| Policy | Resources |
|--------|-----------|
| 1 - namespace | `Namespace` platform-automation |
| 2 - token-review | `ServiceAccount` + `ClusterRoleBinding` (system:auth-delegator) + long-lived `Secret` token |
| 3 - operator-rbac | `ServiceAccount` + `ClusterRole` + `ClusterRoleBinding` for the operator |
| 4 - operator-deployment | `Deployment` with projected service account token (PSAT, 1h expiry, audience: `platform-services`) |

**Hub** (label: `local-cluster`):

| Policy | Resources |
|--------|-----------|
| 5 - cluster-views | Dynamically creates one `ManagedClusterView` per operator cluster to read back the `token-review-sa-token` Secret |

**Service cluster** (label: `internal-trust=service`):

| Policy | Resources |
|--------|-----------|
| 6 - namespace | `Namespace` platform-services + `ServiceAccount` |
| 7 - deployment | `Deployment` (2 replicas, HA) + `Service` on port 8443 |
| 8 - token-propagation | Dynamically creates per-cluster `Secrets` (review token + CA + API URL) + `ConfigMap` cluster-registry |
| 9 - sync-status | Monitors `token-sync-ready` ConfigMap (inform canary) |

### Step 10: Label clusters

```bash
# Operator clusters (can be multiple)
oc label managedcluster <operator-cluster-1> internal-trust=operator vendor=OpenShift --overwrite
oc label managedcluster <operator-cluster-2> internal-trust=operator vendor=OpenShift --overwrite

# Service cluster (typically one)
oc label managedcluster <service-cluster> internal-trust=service vendor=OpenShift --overwrite
```

New operator clusters labeled later are **automatically picked up** within ~10 minutes (the policy evaluation interval).

### Step 11: Build and push internal trust images

You need two container images:
1. **dataclassification-agent** — the operator that runs on operator clusters
2. **platform-integration-service** — the central service that validates PSATs via TokenReview

```bash
# Build and push your operator and service images
podman push registry.mydomain.no/platform/dataclassification-agent:latest
podman push registry.mydomain.no/platform/integration-service:latest
```

---

## Deploying with GitOps / ArgoCD

Instead of deploying manually with `kustomize build | oc apply`, you can use
ArgoCD (OpenShift GitOps) to continuously sync all policies from Git. This is
the **recommended production approach** — it provides drift detection, automatic
reconciliation, and a single source of truth.

### Prerequisites for ArgoCD Deployment

1. **OpenShift GitOps operator** installed on the ACM hub cluster
2. **PolicyGenerator Kustomize plugin** available to the ArgoCD repo-server

There are two ways to make the PolicyGenerator plugin available:

**Option A — OpenShift GitOps 1.12+ (easiest)**

The PolicyGenerator plugin is pre-installed. Enable it in the ArgoCD CR:

```yaml
apiVersion: argoproj.io/v1beta1
kind: ArgoCD
metadata:
  name: openshift-gitops
  namespace: openshift-gitops
spec:
  kustomizeBuildOptions: "--enable-alpha-plugins"
```

**Option B — ConfigManagementPlugin sidecar**

Apply the plugin ConfigMap included in this repo, then configure the repo-server
to use it as a sidecar:

```bash
oc apply -f argocd/policygenerator-plugin.yaml
```

Then patch the ArgoCD repo-server Deployment to add a sidecar container that
uses this ConfigMap. Refer to the
[ArgoCD CMP documentation](https://argo-cd.readthedocs.io/en/stable/operator-manual/config-management-plugins/)
for sidecar setup details.

### Repository Structure for GitOps

```
argocd/
├── app-of-apps.yaml              # Parent Application (deploy this one)
├── applicationset.yaml           # Alternative: ApplicationSet approach
├── policygenerator-plugin.yaml   # ArgoCD ConfigManagementPlugin for PolicyGenerator
├── README.md
└── applications/                 # Child Applications (managed by app-of-apps)
    ├── kustomization.yaml
    ├── 00-shared.yaml            # Wave 0: Placements + ClusterSetBinding
    ├── 01-cert-manager.yaml      # Wave 1: cert-manager operator
    ├── 02-external-policies.yaml # Wave 2: OIDC external trust
    └── 03-internal-policies.yaml # Wave 2: Internal cross-cluster trust
```

### Approach A: App of Apps (recommended)

The **App of Apps** pattern uses a single parent ArgoCD Application that deploys
four child Applications — one per policy domain. Sync waves control the ordering:

```
Wave 0: shared resources (Placements, ManagedClusterSetBinding)
    │
    ▼
Wave 1: cert-manager operator (prerequisite for external trust TLS)
    │
    ▼
Wave 2: external trust + internal trust (independent, deploy in parallel)
```

**Step 1: Replace the Git repository URL**

Before deploying, update `repoURL` in all ArgoCD manifests:

```bash
# Replace the placeholder URL in all ArgoCD Application files
find argocd/ -name '*.yaml' -exec sed -i \
  's|https://git.mydomain.no/platform/acm-workload-identity.git|https://your-git-server.com/your-org/your-repo.git|g' {} +
```

**Step 2: Create the AAP credentials secret**

ArgoCD cannot manage Secrets with sensitive data from Git. Apply this manually:

```bash
cp acm_external_policies/secrets/aap-credentials-secret.yaml.template \
   acm_external_policies/secrets/aap-credentials-secret.yaml
# Edit with real values
vi acm_external_policies/secrets/aap-credentials-secret.yaml
oc apply -f acm_external_policies/secrets/aap-credentials-secret.yaml -n acm-policies
```

**Step 3: Deploy the App of Apps**

```bash
oc apply -f argocd/app-of-apps.yaml
```

This single command triggers the full deployment chain:

1. ArgoCD creates the parent Application `acm-workload-identity` in `openshift-gitops`
2. The parent syncs `argocd/applications/` and creates 4 child Applications
3. **Wave 0** — `acm-shared-resources` syncs `shared/` directory:
   - Creates `ManagedClusterSetBinding` and all 5 `Placement` objects in `acm-policies`
4. **Wave 1** — `acm-cert-manager-policies` syncs `acm_cert_manager/`:
   - Runs PolicyGenerator via `kustomize build --enable-alpha-plugins`
   - Creates cert-manager operator policies, ClusterIssuer, health checks
5. **Wave 2** — Two Applications sync in parallel:
   - `acm-oidc-external-policies` syncs `acm_external_policies/`:
     Creates 8 OIDC policies, 2 PolicySets, 2 PolicyAutomations
   - `acm-internal-trust-policies` syncs `acm_internal_policies/`:
     Creates 9 internal trust policies, 3 PolicySets

**Step 4: Label clusters to activate policies**

Policies are deployed but dormant until clusters are labeled:

```bash
# OIDC external trust
oc label managedcluster <cluster-name> oidc-federation=enabled vendor=OpenShift --overwrite

# Internal trust — operator clusters
oc label managedcluster <cluster-name> internal-trust=operator vendor=OpenShift --overwrite

# Internal trust — service cluster
oc label managedcluster <cluster-name> internal-trust=service vendor=OpenShift --overwrite
```

**Step 5: Ongoing operations**

From this point, all changes are made via Git:
- Push a manifest change → ArgoCD detects drift → auto-syncs within 3 minutes
- If someone manually modifies a policy on the hub → ArgoCD self-heals it back
- To add a new policy → add it to the PolicyGenerator config → push → ArgoCD syncs
- To remove a domain → delete the child Application YAML → push → ArgoCD prunes

### Approach B: ApplicationSet (alternative)

If you prefer a single resource instead of parent + children, use the
`ApplicationSet` with a list generator:

```bash
# Shared resources must be deployed first (no sync wave support in ApplicationSets)
kustomize build shared/ | oc apply -f - -n acm-policies

# Then deploy the ApplicationSet
oc apply -f argocd/applicationset.yaml
```

The ApplicationSet generates 4 Applications from a list:

| Generated Application | Path | Plugin |
|----------------------|------|--------|
| `acm-shared-resources` | `shared` | kustomize-policygenerator |
| `acm-cert-manager-policies` | `acm_cert_manager` | kustomize-policygenerator |
| `acm-oidc-external-policies` | `acm_external_policies` | kustomize-policygenerator |
| `acm-internal-trust-policies` | `acm_internal_policies` | kustomize-policygenerator |

> **Note**: ApplicationSets do not natively support sync wave ordering between
> generated Applications. If ordering is critical (it usually is for the first
> deployment), use the App of Apps approach or deploy shared resources manually
> before applying the ApplicationSet.

### Approach C: Individual Applications

You can also deploy individual Applications for specific domains without the
parent:

```bash
# Deploy only what you need
oc apply -f argocd/applications/00-shared.yaml
oc apply -f argocd/applications/01-cert-manager.yaml
oc apply -f argocd/applications/02-external-policies.yaml
oc apply -f argocd/applications/03-internal-policies.yaml
```

This is useful when:
- You only need one policy domain (e.g., only internal trust)
- You want to manage each domain in a separate ArgoCD project
- You need different sync policies per domain

### ArgoCD Dashboard View

After deployment, the ArgoCD dashboard shows:

```
acm-workload-identity (parent)
├── acm-shared-resources           Synced ✓  Healthy ✓
├── acm-cert-manager-policies      Synced ✓  Healthy ✓
├── acm-oidc-external-policies     Synced ✓  Healthy ✓
└── acm-internal-trust-policies    Synced ✓  Healthy ✓
```

Each child Application shows the individual ACM policies it manages. The ACM
Governance dashboard shows the policy compliance status per managed cluster.

### Troubleshooting GitOps Deployment

**ArgoCD shows "ComparisonError" or "Unknown" status**

The PolicyGenerator plugin is not available to the repo-server. Verify:
```bash
# Check repo-server logs
oc logs -n openshift-gitops deployment/openshift-gitops-repo-server | grep -i policy

# Verify the plugin is configured
oc get configmap -n openshift-gitops kustomize-policygenerator -o yaml
```

**Sync succeeds but no policies appear in `acm-policies` namespace**

Check that the ArgoCD Application `destination.namespace` is `acm-policies` and
that the namespace exists:
```bash
oc get namespace acm-policies
oc get policy -n acm-policies
```

**Policies appear but are not applied to clusters**

Placements have no matching clusters. Verify labels:
```bash
oc get placement -n acm-policies -o wide
oc get managedcluster --show-labels
```

**Wave ordering not working (cert-manager deploys before shared)**

Ensure the parent Application's destination is `openshift-gitops` (not
`acm-policies`). Sync waves apply to resources within the parent's sync —
the child Application objects, not their contents.

---

## Verification

### ACM Policy Status

```bash
# Check all policies on the hub
oc get policy -n acm-policies

# Check PolicySets
oc get policyset -n acm-policies

# Check placements and which clusters are selected
oc get placement -n acm-policies -o wide

# Check ManagedClusterViews (internal trust)
oc get managedclusterview -A

# Check policy compliance
oc get policy -n acm-policies -o custom-columns=NAME:.metadata.name,COMPLIANT:.status.compliant

# Check PolicyAutomation status
oc get policyautomation -n acm-policies
```

### ArgoCD Status (GitOps deployment only)

```bash
# Check all ArgoCD Applications
oc get applications -n openshift-gitops

# Check sync status of the parent app
oc get application acm-workload-identity -n openshift-gitops -o jsonpath='{.status.sync.status}'

# Check health of all child apps
oc get applications -n openshift-gitops \
  -o custom-columns=NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status

# Check for sync errors
oc get applications -n openshift-gitops -o json | \
  jq -r '.items[] | select(.status.conditions) | "\(.metadata.name): \(.status.conditions[].message)"'

# Manually trigger a sync (if auto-sync is disabled)
oc patch application acm-workload-identity -n openshift-gitops \
  --type merge -p '{"operation":{"sync":{"revision":"HEAD"}}}'
```

---

## Architecture Summary

```
┌─────────────────────────────────────────────────────┐
│                    ACM Hub                           │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────┐ │
│  │ PolicySets   │  │ Placements   │  │ ClusterView│ │
│  │ (5 sets)     │  │ (5 rules)    │  │ (dynamic) │ │
│  └──────────────┘  └──────────────┘  └───────────┘ │
└──────────┬──────────────────┬───────────────┬───────┘
           │                  │               │
    ┌──────▼──────┐   ┌──────▼──────┐  ┌─────▼──────┐
    │ Operator    │   │ Service     │  │ OIDC       │
    │ Clusters    │   │ Cluster     │  │ Clusters   │
    │             │   │             │  │            │
    │ - PSAT      │   │ - TokenReview│ │ - Proxy    │
    │ - Agent     │   │ - Registry  │  │ - Route    │
    │ - Review SA │   │ - Secrets   │  │ - Certs    │
    └─────────────┘   └─────────────┘  └────────────┘
                                              │
                                        ┌─────▼──────┐
                                        │ External   │
                                        │ - F5 VIP   │
                                        │ - DNS      │
                                        │ - Azure AD │
                                        └────────────┘
```

**Cluster labels drive everything** — adding/removing labels is how you onboard/offboard clusters. The PolicyGenerator + hub templates + ManagedClusterView pattern handles dynamic scaling automatically.

### Internal Trust — Direction of Authentication

The internal trust is **one-directional: spoke → hub**.

```
cluster1 (spoke)                              hub cluster
┌──────────────────────────┐                  ┌──────────────────────────────────┐
│ dataclassification-agent │                  │ platform-integration-service     │
│ pod                      │                  │                                  │
│                          │  1. sends PSAT   │ 2. receives PSAT                 │
│                          │ ──────────────►  │                                  │
│                          │                  │ 3. calls TokenReview on cluster1 │
│ kube-apiserver           │  ◄────────────── │    using review-token-cluster1   │
│                          │  4. validates    │                                  │
│                          │ ──────────────►  │ 5. authenticated=true            │
│                          │                  │    → trusts the caller            │
└──────────────────────────┘                  └──────────────────────────────────┘
```

**How it works:**

1. The operator pod on cluster1 mounts a projected SA token (PSAT) with `audience: "platform-services"` and `expirationSeconds: 3600`
2. The pod sends this PSAT to the central service on the hub cluster (via the `Authorization` header)
3. The hub service reads `review-token-cluster1` Secret (contains: token-review-sa credentials, CA cert, API URL)
4. The hub service calls cluster1's TokenReview API using those credentials, passing the operator's PSAT
5. cluster1's API validates the token and returns the identity: `system:serviceaccount:platform-automation:dataclassification-agent`
6. The hub service now trusts the caller

**What does NOT work with the current configuration:**

- A pod on the **hub** cannot authenticate to a pod on **cluster1** — there are no review credentials for the hub stored on cluster1
- The trust is strictly spoke-to-hub because only spoke clusters are labeled `internal-trust=operator` and have the token-review infrastructure deployed

**To enable bidirectional trust** (if needed):

- Label the hub's ManagedCluster (`local-cluster`) with `internal-trust=operator`
- The same policy framework will automatically deploy the token-review SA, Secret, and ConfigMap mirror on the hub
- Run a receiving service on cluster1 labeled `internal-trust=service`
- The hub templates will propagate the hub's review credentials to cluster1

---

## Deployment Fixes Log (ACM 2.15)

The following fixes were applied during real deployment to ACM 2.15.1. These
address compatibility issues, missing resources, and ACM 2.15 behavioral changes.

### FIX #1 — Missing PlacementBindings

**Problem**: PolicyGenerator with `generatePolicyPlacement: false` does NOT
generate PlacementBindings. Policies are created but never distributed to clusters.

**Fix**: Created `shared/placement-bindings.yaml` with 6 PlacementBindings that
connect each PolicySet/Policy to its corresponding Placement.

**File**: `shared/placement-bindings.yaml` (added to `shared/kustomization.yaml`)

### FIX #2 — cert-manager operator channel mismatch

**Problem**: The OperatorPolicy specified channel `stable-v1.17`, but the spoke
cluster (OCP 4.18) only offers `stable-v1` and `stable-v1.18`. Subscription fails
with "constraints not satisfiable".

**Fix**: Changed channel to `stable-v1.18` in `acm_cert_manager/manifests/operatorpolicy.yml`.
Always verify the available channels on your target clusters:

```bash
oc get packagemanifest openshift-cert-manager-operator -n openshift-marketplace \
  -o jsonpath='{range .status.channels[*]}{.name}{"\n"}{end}'
```

**File**: `acm_cert_manager/manifests/operatorpolicy.yml`

### FIX #3 — CA ClusterIssuer placeholder values

**Problem**: The CA secret contained literal placeholder strings
`<REPLACE: base64-encoded CA certificate>` which fail base64 validation.

**Fix**: Generated a self-signed CA and replaced the placeholders with real
base64-encoded certificate and key data.

**File**: `acm_cert_manager/manifests/ca-clusterissuer/ca-clusterissuer-secret.yml`

### FIX #4 — Governance framework hub template RBAC

**Problem**: The governance-policy-framework addon SA on local-cluster lacked
permissions to lookup `ManagedCluster` and `ManagedClusterView` resources needed
by hub templates.

**Fix**: Created `shared/governance-hub-template-rbac.yaml` with a ClusterRole
and ClusterRoleBinding granting get/list/watch on ManagedCluster and
ManagedClusterView to the `governance-policy-framework-sa` SA.

**File**: `shared/governance-hub-template-rbac.yaml`

### FIX #5 — ACM 2.15 hub template lookup restrictions

**Problem**: ACM 2.15 restricts hub template (`{{hub ... hub}}`) lookups to:
1. Only **namespaced** resources (cluster-scoped lookups like ManagedCluster are blocked)
2. Only resources in the **policy's own namespace** (cross-namespace lookups blocked)

The original token-propagation policy used hub templates to iterate ManagedClusters
(cluster-scoped) and read ManagedClusterViews (cross-namespace).

**Fix**: Restructured into a two-phase approach:
- **Policy A** (local-cluster, uses regular templates with no restrictions):
  Creates ManagedClusterViews AND copies all data (tokens, CA certs, API URLs)
  into the `acm-policies` namespace as Secrets and a registry ConfigMap.
- **Policy B** (service cluster, uses hub templates):
  Reads only from `acm-policies` namespace (same namespace = allowed).

**Files**:
- `acm_internal_policies/manifests/hub-side/01-managedclusterview-creator.yaml`
- `acm_internal_policies/manifests/service-side/06-token-propagation.yaml`

### FIX #6 — ACM 2.15 blocks ManagedClusterView for Secrets

**Problem**: ACM 2.15 blocks `ManagedClusterView` from reading Secrets entirely
(`ResourceTypeInvalid: viewing secrets is not allowed`). This is a hardcoded
security restriction, not an RBAC issue.

**Fix**: Created a ConfigurationPolicy template
(`04b-tokenreview-token-configmap.yaml`) that reads the service-account-token
Secret on the operator cluster and mirrors its data into a ConfigMap
(`token-review-sa-mirror`). The ManagedClusterView then reads the ConfigMap
instead of the Secret.

**Files**:
- `acm_internal_policies/manifests/operator-side/04b-tokenreview-token-configmap.yaml` (new)
- `acm_internal_policies/manifests/hub-side/01-managedclusterview-creator.yaml` (updated scope)
- `acm_internal_policies/policy-generator-config.yaml` (added manifest reference)

### FIX #7 — YAML parsing error from multi-line PEM certificates

**Problem**: The token-propagation policy used `stringData` with `base64dec` to
decode hub-copy Secret values. PEM certificates contain newlines and `-----BEGIN`
markers that break YAML single-line string formatting when decoded.

**Fix**: Changed from `stringData` + `base64dec` to `data` with raw base64 values.
Since the data is already base64-encoded in `.data`, passing it directly to the
target Secret's `.data` preserves encoding without YAML issues.

**File**: `acm_internal_policies/manifests/service-side/06-token-propagation.yaml`

### FIX #8 — OIDC proxy image not built / missing go.mod

**Problem**: The `oidc-proxy-deployment` policy was NonCompliant because the
deployment referenced `registry.mydomain.no/oidc-proxy:latest`, an image that
didn't exist. The Dockerfile was also missing `go.mod` in the COPY step, causing
the Go build to fail.

**Fix**:
1. Created `reference-images/oidc-proxy/go.mod` (required for Go modules)
2. Updated `Dockerfile` to `COPY go.mod main.go ./`
3. Built the image with `podman build`
4. Exposed the spoke cluster's internal registry (`defaultRoute: true`)
5. Pushed the image to `openshift-oidc-proxy/oidc-proxy:latest` in the internal registry
6. Updated `acm_external_policies/manifests/05-deployment.yaml` to use
   `image-registry.openshift-image-registry.svc:5000/openshift-oidc-proxy/oidc-proxy:latest`

**Files**:
- `reference-images/oidc-proxy/go.mod` (new)
- `reference-images/oidc-proxy/Dockerfile` (updated COPY)
- `acm_external_policies/manifests/05-deployment.yaml` (updated image reference)

### FIX #9 — API URL claim name mismatch

**Problem**: The hub-side policy (`01-managedclusterview-creator.yaml`) extracted the
cluster API URL from the ManagedCluster claim `kubeapiserver.open-cluster-management.io`,
but OpenShift clusters expose this as `apiserverurl.openshift.io` instead. This caused the
`api-url` field in `hub-copy-token-*` and `review-token-*` Secrets to be empty, breaking
the service's ability to know which API endpoint to call for TokenReview.

**Fix**: Changed the claim lookup to match on either claim name using `or`:
```
{{- if or (eq $claim.name "kubeapiserver.open-cluster-management.io") (eq $claim.name "apiserverurl.openshift.io") }}
```

**File**: `acm_internal_policies/manifests/hub-side/01-managedclusterview-creator.yaml`

### Pre-deployment checklist (ACM 2.15)

Before deploying to an ACM 2.15 cluster, ensure:

1. [ ] `ManagedClusterSet` `all-openshift-clusters` exists and contains all clusters
2. [ ] `acm-policies` namespace exists
3. [ ] cert-manager operator channel matches your OCP version
4. [ ] CA secret has real base64-encoded cert+key (not placeholders)
5. [ ] Container images are built and pushed (oidc-proxy, agent, service)
6. [ ] `governance-hub-template-rbac.yaml` is applied on the hub
7. [ ] PlacementBindings are deployed (not generated by PolicyGenerator)
