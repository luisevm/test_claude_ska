# Shared ACM Resources

Common resources used by all policy domains. **Deploy this first** before any
of the individual policy sets.

## What it creates

| Resource | Name | Purpose |
|----------|------|---------|
| **ManagedClusterSetBinding** | `all-openshift-clusters` | Binds the cluster set to the `acm-policies` namespace, required for all Placements |
| **Placement** | `placement-oidc-federation` | Selects clusters with `oidc-federation=enabled` + `vendor=OpenShift` (used by `acm_cert_manager` and `acm_external_policies`) |
| **Placement** | `placement-internal-trust-operator` | Selects clusters with `internal-trust=operator` + `vendor=OpenShift` |
| **Placement** | `placement-internal-trust-service` | Selects clusters with `internal-trust=service` + `vendor=OpenShift` |
| **Placement** | `placement-hub` | Selects `local-cluster` (the ACM hub) |

## Namespace

All resources live in the `acm-policies` namespace, which is shared by all
three policy domains.

## Deployment

### Via ArgoCD (recommended)

Use the centralized App of Apps pattern from the repository root, which deploys
shared resources first (sync wave 0) before any policy domains:

```bash
oc apply -f argocd/app-of-apps.yaml
```

### Via manual kustomize

```bash
kustomize build . | oc apply -f - -n acm-policies
```

Deploy this **before** any of the policy domain Applications:
1. `shared/` (this — sync wave 0)
2. `acm_cert_manager/` (sync wave 1)
3. `acm_external_policies/` (sync wave 2)
4. `acm_internal_policies/` (sync wave 2)
