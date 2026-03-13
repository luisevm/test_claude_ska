# ACM Internal Trust for Platform Services

Automates the setup of cross-cluster trust between operators and a centralized
platform service using opaque Projected Service Account Tokens (PSATs) with
TokenReview-based authentication. Fully managed by ACM PolicyGenerator — no
Ansible or external automation systems needed.

## Architecture

```
OPERATOR CLUSTERS (labeled: internal-trust=operator)
┌───────────────────────────────────────────────────┐
│  Namespace: platform-automation                   │
│                                                   │
│  ServiceAccount: token-review-sa                  │
│    └─ ClusterRoleBinding → system:auth-delegator  │
│    └─ Secret: token-review-sa-token (long-lived)  │
│                                                   │
│  ServiceAccount: dataclassification-agent         │
│    └─ Deployment: projected SA token (PSAT)       │
│       audience: "platform-services", TTL: 1hr     │
└───────────┬───────────────────────────────────────┘
            │
            │ ManagedClusterView reads Secret
            │ back to the ACM hub
            ▼
ACM HUB (local-cluster)
┌───────────────────────────────────────────────────┐
│  Policy 5 (object-templates-raw):                 │
│    Iterates ManagedClusters with                  │
│    internal-trust=operator label                  │
│    Creates ManagedClusterView per cluster          │
│    → reads token-review-sa-token Secret           │
│    → stores in .status.result on hub              │
│                                                   │
│  Policy 8 (hub templates + object-templates-raw): │
│    Reads ManagedClusterView .status.result        │
│    Creates per-cluster Secrets on service cluster │
│    Creates cluster-registry ConfigMap             │
└───────────┬───────────────────────────────────────┘
            │
            │ Hub templates propagate token data
            │ to the service cluster via ACM policy
            ▼
SERVICE CLUSTER (labeled: internal-trust=service)
┌───────────────────────────────────────────────────┐
│  Namespace: platform-services                     │
│                                                   │
│  Secret: review-token-<cluster-a>                 │
│  Secret: review-token-<cluster-b>                 │
│  Secret: review-token-<cluster-c>                 │
│    (one per operator cluster, created by Policy 8)│
│                                                   │
│  ConfigMap: cluster-registry                      │
│    (comma-separated list of cluster names)        │
│                                                   │
│  Deployment: platform-integration-service         │
│    Receives opaque PSATs from operators           │
│    Performs TokenReview using per-cluster tokens   │
│                                                   │
│  ConfigMap: token-sync-ready (feedback signal)    │
└───────────────────────────────────────────────────┘
```

## Repository Structure

```
acm-internal-trust/
├── kustomization.yaml
├── policy-generator-config.yaml
├── placement-operator-clusters.yaml       # Selects: internal-trust=operator
├── placement-service-cluster.yaml         # Selects: internal-trust=service
├── placement-hub.yaml                     # Selects: local-cluster (ACM hub)
├── managed-cluster-set-binding.yaml
│
├── manifests/
│   ├── operator-side/                     # → ALL operator clusters
│   │   ├── 01-namespace.yaml
│   │   ├── 02-tokenreview-serviceaccount.yaml
│   │   ├── 03-tokenreview-clusterrolebinding.yaml  (→ system:auth-delegator)
│   │   ├── 04-tokenreview-token-secret.yaml         (long-lived, auto-populated)
│   │   ├── 05-operator-serviceaccount.yaml
│   │   ├── 06-operator-clusterrole.yaml
│   │   ├── 07-operator-clusterrolebinding.yaml
│   │   └── 08-operator-deployment.yaml              (projected SA token)
│   │
│   ├── hub-side/                          # → local-cluster (ACM hub)
│   │   └── 01-managedclusterview-creator.yaml   (object-templates-raw)
│   │
│   └── service-side/                      # → SERVICE cluster
│       ├── 01-namespace.yaml
│       ├── 02-serviceaccount.yaml
│       ├── 03-deployment.yaml
│       ├── 04-service.yaml
│       ├── 05-token-sync-check.yaml               (canary)
│       └── 06-token-propagation.yaml               (hub templates + object-templates-raw)
```

## How It Works — Pure ACM, No Ansible

### Step 1: Label operator clusters

```bash
oc label managedcluster <cluster-name> internal-trust=operator --overwrite
```

ACM deploys Policies 1-4: namespace, token-review SA + `system:auth-delegator`
binding, long-lived token Secret, operator SA + RBAC, operator Deployment.

### Step 2: Label the service cluster

```bash
oc label managedcluster <cluster-name> internal-trust=service --overwrite
```

ACM deploys Policies 6-9: namespace, ServiceAccount, Deployment, Service,
token propagation, canary check.

### Step 3: Automatic cross-cluster token propagation

1. **Policy 5** (hub): `object-templates-raw` iterates all `internal-trust=operator`
   ManagedClusters, creates a `ManagedClusterView` per cluster that reads the
   `token-review-sa-token` Secret. The Secret data appears in
   `.status.result` on the hub.

2. **Policy 8** (service cluster): `object-templates-raw` with **hub templates**
   iterates all ManagedClusterViews, reads token data from `.status.result`,
   and creates:
   - `Secret: review-token-<clustername>` per operator cluster
   - `ConfigMap: cluster-registry` with the list of cluster names
   - `ConfigMap: token-sync-ready` (feedback signal)

3. **Policy 9** (canary): Confirms `token-sync-ready` exists. Reports Compliant
   once propagation is complete.

### Adding a new operator cluster later

```bash
oc label managedcluster <new-cluster> internal-trust=operator --overwrite
```

On the next evaluation cycle (≤5 min for noncompliant, ≤30 min for compliant):
- Policy 5 creates a new ManagedClusterView for the new cluster
- Policy 8 picks up the new view and creates `review-token-<new-cluster>` Secret
- No manual re-trigger needed — the policies self-heal

## Key Design: Why This Works Without Ansible

The external trust (OIDC/Entra) needs Ansible because it configures **non-Kubernetes
systems** (F5 BIG-IP, DNS servers, Azure Entra ID). ACM policies cannot talk to those.

The internal trust is **entirely Kubernetes objects**. The only challenge is
cross-cluster Secret propagation, which is solved by:

1. `ManagedClusterView` — bridges managed cluster data to the hub
2. Hub templates (`{{hub ... hub}}`) — reads hub-side data during policy evaluation
3. `object-templates-raw` — enables dynamic iteration over an unknown number of clusters

## What You MUST Customize

| File | What to change |
|------|---------------|
| `manifests/operator-side/06-operator-clusterrole.yaml` | Operator permissions for your workload |
| `manifests/operator-side/08-operator-deployment.yaml` | Operator container image, service URL |
| `manifests/service-side/03-deployment.yaml` | Service container image |

## What I Cannot Fully Confirm

- Whether `ManagedClusterView.status.result` preserves Secret `.data` fields
  with base64 encoding intact through the hub-template evaluation chain.
  **Test this before production use.**
- Exact RBAC requirements for the policy controller to read ManagedClusterView
  status on the hub. Standard ACM installs should have this, but verify.
- Hub template re-evaluation timing: new clusters are picked up within the
  `evaluationInterval` period (5 min noncompliant / 30 min compliant).

## API Versions Used

| Resource | API Version |
|----------|------------|
| Placement | `cluster.open-cluster-management.io/v1beta1` |
| ManagedClusterSetBinding | `cluster.open-cluster-management.io/v1beta2` |
| ManagedClusterView | `view.open-cluster-management.io/v1beta1` |
| PolicyGenerator | `policy.open-cluster-management.io/v1` |
| ConfigurationPolicy | `policy.open-cluster-management.io/v1` |

## Deployment

### Option A: ArgoCD / OpenShift GitOps (recommended)

Use the centralized App of Apps pattern from the repository root, which deploys
all policy domains (shared, cert-manager, external, internal) with correct
ordering via sync waves:

```bash
oc apply -f argocd/app-of-apps.yaml
```

Alternatively, deploy only the internal trust policies independently:

```bash
oc apply -f argocd/applications/03-internal-policies.yaml
```

See [DEPLOYMENT_GUIDE.md](../DEPLOYMENT_GUIDE.md) for the full GitOps walkthrough.

### Option B: Manual Kustomize Build

```bash
kustomize build --enable-alpha-plugins . | oc apply -f - -n acm-policies
```
