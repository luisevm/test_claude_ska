# ArgoCD / GitOps Deployment

This directory contains ArgoCD manifests for deploying all ACM workload identity
policies via GitOps.

## Deployment Approaches

| Approach | File | Best for |
|----------|------|----------|
| **App of Apps** (recommended) | `app-of-apps.yaml` | Full control with sync wave ordering |
| **ApplicationSet** | `applicationset.yaml` | Simpler single-resource management |

## Quick Start

```bash
# 1. Configure the PolicyGenerator plugin in ArgoCD (one-time setup)
oc apply -f argocd/policygenerator-plugin.yaml

# 2. Deploy using App of Apps
oc apply -f argocd/app-of-apps.yaml
```

See [DEPLOYMENT_GUIDE.md](../DEPLOYMENT_GUIDE.md) for the full walkthrough.

## Directory Structure

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

## Sync Wave Ordering

```
Wave 0: shared resources (Placements, ManagedClusterSetBinding)
    │
    ▼
Wave 1: cert-manager operator (prerequisite for external trust TLS)
    │
    ▼
Wave 2: external trust + internal trust (independent, deploy in parallel)
```
