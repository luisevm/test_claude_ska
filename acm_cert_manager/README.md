# ACM Policy: cert-manager Operator Installation

Installs the cert-manager Operator for Red Hat OpenShift on managed clusters
using the ACM Policy Framework with `OperatorPolicy` and `PolicyGenerator`.

Adapted from [acm-gitops-demo](https://github.com/csa-rh/acm-gitops-demo.git)
(`operators/cert-manager/`).

## What Gets Deployed

| # | Resource | Purpose |
|---|----------|---------|
| 1 | **Namespace** (`cert-manager-operator`) | Operator namespace with cluster monitoring |
| 2 | **Namespace** (`cert-manager`) | Workload namespace for cert-manager components |
| 3 | **ConfigMap** (`trusted-ca`) | Injects cluster trusted CA bundle into cert-manager |
| 4 | **OperatorPolicy** | Manages OLM Subscription, OperatorGroup, CSV for `openshift-cert-manager-operator` |
| 5 | **Health check** (inform) | Verifies all cert-manager Deployments are available and ready |
| 6 | **ClusterIssuer** (`ca-clusterissuer`) | Example CA-based issuer (replace for production) |
| 7 | **Secret** (`ca-clusterissuer-tls`) | CA certificate + key for the example issuer (placeholder) |

## Policy Dependencies

```
cert-manager-operator (enforce)
  ├─ Namespace: cert-manager-operator
  ├─ ConfigMap: trusted-ca
  ├─ OperatorPolicy: subscription + OLM lifecycle
  ├─ Health: all Deployments ready (inform only)
  └─ Namespace: cert-manager
        │
        │  depends on: cert-manager-operator = Compliant
        ▼
cert-manager-clusterissuer (enforce)
  ├─ Secret: ca-clusterissuer-tls
  │     │
  │     │  depends on: ca-clusterissuer-secret = Compliant
  │     ▼
  └─ ClusterIssuer: ca-clusterissuer
```

## Prerequisites

- ACM 2.14+ (required for `fail` function in health check template)
- Managed clusters with access to `redhat-operators` CatalogSource

## Repository Structure

```
acm_cert_manager/
├── kustomization.yaml
├── generator.yml                           # PolicyGenerator config
├── placements/
│   ├── placement.yml                       # Selects: oidc-federation=enabled
│   ├── placementbinding.yml
│   └── policyset.yml
└── manifests/
    ├── namespace.yml                       # cert-manager-operator namespace
    ├── cert-manager-namespace.yml          # cert-manager namespace
    ├── trusted-ca-configmap.yml            # Cluster CA bundle injection
    ├── operatorpolicy.yml                  # OLM operator lifecycle
    ├── health/
    │   └── cert-manager-status.yml         # Deployment readiness check
    └── ca-clusterissuer/
        ├── ca-clusterissuer-secret.yml     # CA cert + key (PLACEHOLDER)
        └── ca-clusterissuer.yml            # ClusterIssuer CR
```

## Deployment

This is deployed as a prerequisite before the OIDC external trust policies
(`acm_external_policies`), which depend on cert-manager for TLS certificate
management.

### Via ArgoCD (recommended)

Use the centralized App of Apps pattern from the repository root, which deploys
all policy domains (shared, cert-manager, external, internal) with correct
ordering via sync waves:

```bash
oc apply -f argocd/app-of-apps.yaml
```

Alternatively, deploy only the cert-manager policies independently:

```bash
oc apply -f argocd/applications/01-cert-manager.yaml
```

See [DEPLOYMENT_GUIDE.md](../DEPLOYMENT_GUIDE.md) for the full GitOps walkthrough.

### Via manual kustomize

```bash
kustomize build --enable-alpha-plugins . | oc apply -f - -n acm-policies
```

## What You MUST Customize

| File | What to change |
|------|---------------|
| `manifests/operatorpolicy.yml` | Verify `channel: stable-v1.17` matches your OCP version |
| `manifests/ca-clusterissuer/ca-clusterissuer-secret.yml` | Replace placeholder with your actual CA cert + key |
| `placements/placement.yml` | Adjust label selector if not using `oidc-federation=enabled` |

## Relationship to Other version1 Components

```
acm_cert_manager (this repo)          acm_external_policies
  │                                     │
  │ Deploys cert-manager operator       │ Deploys OIDC proxy + policies
  │ + ClusterIssuer on clusters         │ Policy 6 creates cert-manager Certificate CR
  │                                     │ (requires cert-manager to be installed)
  │                                     │
  └─── PREREQUISITE for ────────────────┘
```

## Source

Configuration adapted from:
- Path: `acm-gitops-demo/operators/cert-manager/`
- Key changes: Placement uses `oidc-federation=enabled` label (consistent with
  external trust), CA secret uses placeholder values instead of demo cert.

## Notes

- The `OperatorPolicy` manages the full OLM lifecycle: Subscription, OperatorGroup,
  CSV, and CRDs. It does not use a separate Subscription manifest.
- The trusted-ca ConfigMap uses `config.openshift.io/inject-trusted-cabundle: 'true'`
  to automatically inject the cluster's trusted CA bundle. This is harmless if no
  custom CA is configured via the cluster proxy.
- The health check uses `object-templates-raw` to dynamically verify all Deployments
  in the `cert-manager` namespace without hardcoding deployment names.
