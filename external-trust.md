# ACM Policy Deep Dive: Automating OIDC Workload Identity Onboarding

## Summary — What Gets Deployed

This document automates the full onboarding of OIDC workload identity federation with Azure Entra across managed OpenShift clusters. A single label (`oidc-federation=enabled`) triggers the entire pipeline. The following resources are created and managed:

### In-cluster resources (ACM Policies 1–7 — enforce)

These are deployed automatically by ACM to every labeled cluster:


| #   | Resource                                            | Purpose                                                                                          |
| --- | --------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| 1   | **Namespace** (`openshift-oidc-proxy`)              | Dedicated namespace for the OIDC proxy stack                                                     |
| 2   | **ServiceAccount, ClusterRole, ClusterRoleBinding** | Minimal RBAC — proxy reads `/openid/v1/jwks` and `/.well-known/`* from the API server            |
| 3   | **Deployment + Service**                            | OIDC proxy (2 replicas, HA with pod anti-affinity) — serves the discovery document and JWKS keys |
| 4   | **Route**                                           | TLS-terminated edge Route exposing the proxy as `openshift-oidc-<clustername>.auth.mydomain.no`  |
| 5   | **Authentication CR**                               | Sets `serviceAccountIssuer` to the external OIDC URL (causes API server restart)                 |
| 6   | **cert-manager Certificate**                        | Requests and auto-renews the TLS certificate for the Route                                       |
| 7   | **CertificatePolicy** (inform)                      | Monitors certificate expiry — alerts 30 days before expiration                                   |


### External infrastructure (ACM Policy 8 + PolicyAutomation + Ansible)

These live outside the cluster and are configured by Ansible, triggered automatically when Policy 8 goes NonCompliant:


| Resource                                                           | Where           | Configured by                     |
| ------------------------------------------------------------------ | --------------- | --------------------------------- |
| **F5 BIG-IP** virtual server, pool, health monitor, TLS profile    | F5 appliance    | Ansible (`f5networks.f5_modules`) |
| **DNS A record** (`openshift-oidc-<clustername>.auth.mydomain.no`) | DNS server      | Ansible (provider-specific)       |
| **Azure Entra federated identity credential**                      | Azure AD        | Ansible (`azure.azcollection`)    |
| **ConfigMap** (`oidc-external-infra-ready`) — feedback signal      | Managed cluster | Ansible (`kubernetes.core`)       |


### Prerequisite (separate policy set — `acm_cert_manager/`)


| Resource                                     | Purpose                                                                       |
| -------------------------------------------- | ----------------------------------------------------------------------------- |
| **cert-manager Operator** (`OperatorPolicy`) | Installs and manages the cert-manager operator via OLM on all target clusters |
| **ClusterIssuer**                            | Issues TLS certificates for the OIDC proxy Route (Policy 6 depends on this)   |


> The cert-manager policy set is deployed as a separate ArgoCD Application **before** the OIDC policies. See the "Prerequisite: cert-manager Operator Installation" section below.

### Automation glue


| Component                              | Role                                                                                         |
| -------------------------------------- | -------------------------------------------------------------------------------------------- |
| **PolicyGenerator** (Kustomize plugin) | Wraps raw manifests into ACM Policies + PolicySet                                            |
| **Placement**                          | Selects clusters with `oidc-federation=enabled` + `vendor=OpenShift`                         |
| **PolicyAutomation**                   | Triggers Ansible in AAP when Policy 8 is NonCompliant                                        |
| **PolicyAutomation** (cert expiry)     | Triggers Ansible alert when Policy 7 is NonCompliant                                         |
| **ConfigMap feedback loop**            | Ansible creates ConfigMap after external setup; Policy 8 becomes Compliant; automation stops |


---

## Introduction

The Steps That Need Automating Per New Cluster
Before comparing approaches, let's enumerate what actually needs to happen:

-Cluster-level config — Set serviceAccountIssuer in the Authentication CR
-In-cluster resources — Deploy the OIDC proxy (Deployment, Service, ServiceAccount, RBAC, Route)
-TLS certificate — Provision a cert for openshift-oidc-

.auth.mydomain.no
-DNS record — Create/update the DNS entry pointing to the F5 VIP
-F5 BIG-IP — Create the virtual server, pool, TLS profile, and health monitor
-Azure Entra — Register the federated identity credential with the correct issuer URL

Hybrid Approach (Recommended)
In practice, since the workflow crosses so many boundaries, the most robust approach is a layered one:
In-cluster resources → ACM or GitOps (pick based on what you already run)
If you already have ACM managing your clusters, use ACM PolicySets or ApplicationSets to deploy the OIDC proxy stack. The policy framework adds the compliance guarantee — you know that every managed cluster that carries a specific label will have the correct configuration, and drift will be corrected.
If you're more GitOps-native, use ArgoCD ApplicationSets with a Git generator. Each cluster entry in a config file (or each directory) defines the cluster name, and ApplicationSets automatically create the ArgoCD Application for each cluster.
Either way, the in-cluster pieces (issuer config, RBAC, proxy deployment, Route, cert-manager Certificate) are managed declaratively.
F5 + DNS → Terraform or Ansible (triggered as part of the pipeline)
F5 BIG-IP has both a Terraform provider (for AS3 declarations) and well-supported Ansible collections. DNS likely has a similar story depending on your provider.
You'd define a Terraform module or Ansible playbook that takes the cluster name as input and creates the VIP, pool, TLS profile, and DNS record. This gets called as part of the cluster onboarding pipeline.
Azure Entra → Terraform
The azurerm or azuread Terraform provider handles federated identity credential registration cleanly. A module takes the cluster name, constructs the issuer URL, and registers it with the correct app registration.
Orchestration → A pipeline that ties it all together
Something needs to sequence these steps. Options include:

-Tekton / OpenShift Pipelines — fits naturally if you're already in the OpenShift ecosystem. A pipeline triggered by a new cluster event (or a PR merge) runs the Terraform for F5 + DNS + Entra, then either triggers ArgoCD sync or lets ACM policy pick it up.
-Ansible Automation Platform (AAP) — if your org is Ansible-heavy, a workflow template in AAP can orchestrate the full chain: configure F5, create DNS, register in Entra, then apply the in-cluster config via an Ansible k8s module or trigger ACM.
-Simple CI/CD (GitLab CI, GitHub Actions, etc.) — a pipeline that runs on merge to a "cluster registry" repo.

ACM can trigger Ansible directly — through three integration points:
PolicyAutomation (recommended for your case) — the PolicyAutomation CR creates an AnsibleJob CR when initiated, which is picked up by the Ansible Automation Platform Resource operator to initiate the Ansible job in the Controller. Red Hat It supports three modes: once, everyEvent, or disabled Red Hat. In once mode, it fires on the first violation then auto-disables. In everyEvent mode, it fires on every compliance state change.
ClusterCurator — you can create prehook and posthook AnsibleJob instances that occur before or after creating or upgrading your clusters. GitHub This is ideal if ACM provisions the clusters via Hive.
AnsibleJob CRs — direct creation for ad-hoc or subscription-based hooks.

The key design pattern added is the ConfigMap feedback loop: ACM deploys an inform policy that checks for a ConfigMap indicating external infra is ready. When it's missing (NonCompliant), PolicyAutomation triggers Ansible. Ansible configures F5, DNS, and Entra, then creates the ConfigMap on the cluster. The policy goes Compliant, and the automation stops firing. This makes the entire onboarding a single label — oidc-federation=enabled — and everything cascades automatically.
The document now includes complete Ansible role structures for F5 (using f5networks.f5_modules), DNS, and Azure Entra federation, plus the certificate expiry alerting automation.

## Overview

This document provides a complete, implementation-ready guide for automating the deployment of the OIDC proxy infrastructure for Azure Entra workload identity federation across all managed OpenShift clusters using Red Hat Advanced Cluster Management (ACM) Policy Framework with the **PolicyGenerator** Kustomize plugin.

> **Important:** This guide uses the current `Placement` API (`cluster.open-cluster-management.io/v1beta1`). The legacy `PlacementRule` (`apps.open-cluster-management.io/v1`) is **deprecated** and must not be used in new configurations. If you have existing policies using `PlacementRule`, see the migration section at the end.

---

## Architecture: What ACM Manages vs. What Remains External

```
┌──────────────────────────────────────────────────────────────────────────┐
│                    ACM HUB CLUSTER                                       │
│                                                                          │
│  PolicySet: oidc-workload-identity                                       │
│  ┌────────────────────────────────────────────────────────────────────┐   │
│  │  Policy 1: oidc-proxy-namespace         (enforce)                 │   │
│  │  Policy 2: oidc-proxy-rbac              (enforce)                 │   │
│  │  Policy 3: oidc-proxy-deployment        (enforce)                 │   │
│  │  Policy 4: oidc-proxy-route             (enforce)                 │   │
│  │  Policy 5: oidc-issuer-config           (enforce)                 │   │
│  │  Policy 6: oidc-cert-manager            (enforce)                 │   │
│  │  Policy 7: oidc-certificate-monitoring  (inform)                  │   │
│  └────────────────────────────────────────────────────────────────────┘   │
│                                                                          │
│  Placement: placement-oidc-workload-identity                             │
│    → selects clusters with label: oidc-federation=enabled                │
│                                                                          │
│  PlacementBinding: binding-oidc-workload-identity                        │
│    → binds Placement to PolicySet                                        │
│                                                                          │
│  ManagedClusterSetBinding: bound to policies namespace                   │
│                                                                          │
└──────────────────────────────────────────────────────────────────────────┘
                            │
               Policies propagated to
                            │
          ┌─────────────────┼─────────────────┐
          ▼                 ▼                 ▼
   ┌────────────┐   ┌────────────┐   ┌────────────┐
   │  Cluster 1  │   │  Cluster 2  │   │  Cluster 3  │
   │  label:     │   │  label:     │   │  label:     │
   │  oidc-fed.. │   │  oidc-fed.. │   │  oidc-fed.. │
   │  =enabled   │   │  =enabled   │   │  =enabled   │
   └────────────┘   └────────────┘   └────────────┘


   EXTERNAL (Ansible Automation Platform — triggered by ACM PolicyAutomation):
   ┌─────────────────────────────────────────────┐
   │  • F5 BIG-IP virtual server + pool          │
   │  • DNS record                               │
   │  • Azure Entra federated identity credential │
   └─────────────────────────────────────────────┘
```

---

## Git Repository Structure

```
acm-oidc-policies/
├── README.md
├── kustomization.yaml                          # Root kustomization
├── policy-generator-config.yaml                # PolicyGenerator CR
├── placement.yaml                              # Placement (not PlacementRule!)
├── managed-cluster-set-binding.yaml            # ManagedClusterSetBinding
│
├── manifests/                                  # Raw K8s manifests (input to PolicyGenerator)
│   ├── 01-namespace.yaml
│   ├── 02-serviceaccount.yaml
│   ├── 03-clusterrole.yaml
│   ├── 04-clusterrolebinding.yaml
│   ├── 05-deployment.yaml
│   ├── 06-service.yaml
│   ├── 07-route.yaml
│   ├── 08-authentication-cr.yaml
│   ├── 09-cert-manager-certificate.yaml
│   └── 10-certificate-policy.yaml
│
└── overlays/                                   # (optional) Per-environment overrides
    ├── production/
    │   ├── kustomization.yaml
    │   └── patches/
    └── staging/
        ├── kustomization.yaml
        └── patches/
```

---

## Prerequisite: cert-manager Operator Installation

The OIDC external trust setup depends on cert-manager for TLS certificate management (Policy 6 creates a `cert-manager.io/v1 Certificate` CR). The cert-manager operator **must be installed on all target clusters before** the OIDC policies are applied.

The cert-manager operator is deployed as a **separate ACM policy set** (`acm_cert_manager/`), following the recommended pattern of one ArgoCD Application per policy domain (see [ACM 2.14 Governance — Policy Deployment](https://docs.redhat.com/en/documentation/red_hat_advanced_cluster_management_for_kubernetes/2.14/html/governance/policy-deployment)).

### What the cert-manager policy deploys


| Resource                                | Purpose                                                                                                                           |
| --------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| **Namespace** (`cert-manager-operator`) | Operator namespace with cluster monitoring enabled                                                                                |
| **Namespace** (`cert-manager`)          | Workload namespace for cert-manager components                                                                                    |
| **ConfigMap** (`trusted-ca`)            | Injects cluster trusted CA bundle into cert-manager via `config.openshift.io/inject-trusted-cabundle`                             |
| **OperatorPolicy**                      | Manages the OLM lifecycle: Subscription (`openshift-cert-manager-operator`, channel `stable-v1.17`), OperatorGroup, CSV, and CRDs |
| **Health check** (inform)               | Uses `object-templates-raw` to verify all cert-manager Deployments are available and ready                                        |
| **ClusterIssuer** (`ca-clusterissuer`)  | Example CA-based issuer — **replace with your production issuer** (e.g., `letsencrypt-prod`)                                      |


### How it works

The cert-manager policy uses ACM's `OperatorPolicy` (API: `policy.open-cluster-management.io/v1beta1`) instead of manually creating Subscription + OperatorGroup manifests. The `OperatorPolicy` manages the full OLM operator lifecycle including installation, upgrades, and removal:

```yaml
apiVersion: policy.open-cluster-management.io/v1beta1
kind: OperatorPolicy
metadata:
  name: cert-manager-operator-install
spec:
  complianceType: musthave
  remediationAction: enforce
  upgradeApproval: Automatic
  subscription:
    channel: stable-v1.17
    name: openshift-cert-manager-operator
    namespace: cert-manager-operator
    source: redhat-operators
    sourceNamespace: openshift-marketplace
    config:
      env:
        - name: TRUSTED_CA_CONFIGMAP_NAME
          value: trusted-ca
```

The `ClusterIssuer` policy has an explicit **dependency** on the `cert-manager-operator` policy — it only deploys after the operator reports Compliant:

```yaml
# In the PolicyGenerator config (generator.yml):
- name: cert-manager-clusterissuer
  dependencies:
    - name: cert-manager-operator
      compliance: Compliant
```

### Deployment

The cert-manager policy set uses the same Placement selector (`oidc-federation=enabled`) as the OIDC policies, so it targets the same clusters. Deploy it as a separate ArgoCD Application **before** the OIDC external trust policies:

```bash
# Deploy cert-manager policies first
oc apply -f acm_cert_manager/argocd-application.yaml

# Then deploy OIDC external trust policies
oc apply -f acm_external_policies/argocd/application.yaml
```

Or via manual kustomize:

```bash
cd acm_cert_manager && kustomize build --enable-alpha-plugins . | oc apply -f -
```

### Repository structure

```
acm_cert_manager/
├── kustomization.yaml
├── generator.yml                              # PolicyGenerator config
├── placements/
│   ├── placement.yml                          # Selects: oidc-federation=enabled
│   ├── placementbinding.yml
│   └── policyset.yml
└── manifests/
    ├── namespace.yml                          # cert-manager-operator namespace
    ├── cert-manager-namespace.yml             # cert-manager namespace
    ├── trusted-ca-configmap.yml               # Cluster CA bundle injection
    ├── operatorpolicy.yml                     # OLM operator lifecycle
    ├── health/
    │   └── cert-manager-status.yml            # Deployment readiness check
    └── ca-clusterissuer/
        ├── ca-clusterissuer-secret.yml        # CA cert + key (PLACEHOLDER — replace)
        └── ca-clusterissuer.yml               # ClusterIssuer CR
```

> **Note:** The `ca-clusterissuer-secret.yml` contains placeholder values. You must supply your own CA certificate and key. For production, replace the example `ca-clusterissuer` with your actual ClusterIssuer (e.g., an ACME issuer for Let's Encrypt, or your internal PKI). The external trust manifests reference `letsencrypt-prod` as the ClusterIssuer name — update either the cert-manager ClusterIssuer name or the references in `acm_external_policies/manifests/07-route.yaml` and `09-cert-manager-certificate.yaml` to match.

> **ACM version requirement:** The health check uses the `fail` template function, which requires ACM 2.14+.

---

## Step 1: Prerequisites — Placement and ManagedClusterSetBinding

Before any policies can be distributed, you need a `ManagedClusterSet` that contains your target clusters, and a `ManagedClusterSetBinding` in the policy namespace. This is a **mandatory requirement** when using `Placement` (and is the key difference from the deprecated `PlacementRule`, which had unrestricted access).

### managed-cluster-set-binding.yaml

```yaml
# ManagedClusterSetBinding — binds the cluster set to the policy namespace
# This allows the Placement in this namespace to select clusters from this set.
# Without this, Placement will find zero clusters.
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSetBinding
metadata:
  name: all-openshift-clusters
  namespace: oidc-policies                        # Must match the policy namespace
spec:
  clusterSet: all-openshift-clusters              # Must match an existing ManagedClusterSet
```

> **Note:** If you use the `default` ManagedClusterSet (which includes all clusters), bind that instead. The key point is that the cluster set must be bound to the namespace where your policies live.

### placement.yaml

```yaml
# Placement — the CURRENT API (replaces deprecated PlacementRule)
# Uses cluster.open-cluster-management.io/v1beta1 (NOT apps.open-cluster-management.io/v1)
apiVersion: cluster.open-cluster-management.io/v1beta1
kind: Placement
metadata:
  name: placement-oidc-workload-identity
  namespace: oidc-policies
spec:
  clusterSets:
    - all-openshift-clusters                      # References the ManagedClusterSet
  predicates:
    - requiredClusterSelector:
        labelSelector:
          matchExpressions:
            - key: oidc-federation
              operator: In
              values:
                - "enabled"
            - key: vendor
              operator: In
              values:
                - "OpenShift"
```

**How auto-onboarding works:** When a new cluster is imported into ACM and you add the label `oidc-federation=enabled`, the Placement automatically includes it. ACM then propagates all associated policies to that cluster. No manual policy deployment needed.

---

## Step 2: Kubernetes Manifests (Policy Payloads)

These are the raw Kubernetes manifests that the PolicyGenerator will wrap into ACM policies. Each file represents a resource that must exist on every managed cluster.

### manifests/01-namespace.yaml

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-oidc-proxy
  labels:
    app.kubernetes.io/name: oidc-proxy
    app.kubernetes.io/part-of: workload-identity
```

### manifests/02-serviceaccount.yaml

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: oidc-proxy-sa
  namespace: openshift-oidc-proxy
```

### manifests/03-clusterrole.yaml

```yaml
# Minimal permissions — ONLY what the proxy needs to read OIDC/JWKS
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: oidc-proxy-reader
rules:
  # Read the JWKS signing keys
  - nonResourceURLs:
      - /openid/v1/jwks
    verbs:
      - get
  # Read the OAuth metadata
  - nonResourceURLs:
      - /.well-known/oauth-authorization-server
      - /.well-known/openid-configuration
    verbs:
      - get
```

### manifests/04-clusterrolebinding.yaml

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: oidc-proxy-reader-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: oidc-proxy-reader
subjects:
  - kind: ServiceAccount
    name: oidc-proxy-sa
    namespace: openshift-oidc-proxy
```

### manifests/05-deployment.yaml

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: oidc-proxy
  namespace: openshift-oidc-proxy
  labels:
    app: oidc-proxy
spec:
  replicas: 2
  selector:
    matchLabels:
      app: oidc-proxy
  template:
    metadata:
      labels:
        app: oidc-proxy
    spec:
      serviceAccountName: oidc-proxy-sa
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                labelSelector:
                  matchLabels:
                    app: oidc-proxy
                topologyKey: kubernetes.io/hostname
      containers:
        - name: oidc-proxy
          # Replace with your actual proxy image
          image: registry.mydomain.no/oidc-proxy:latest
          ports:
            - containerPort: 8080
              name: http
          env:
            # The proxy reads this to construct the discovery document
            # Uses hub template to inject the cluster name dynamically
            - name: ISSUER_URL
              value: '{{ (lookup "config.openshift.io/v1" "Infrastructure" "" "cluster").status.apiServerURL | replace "https://api." "https://openshift-oidc-" | replace ":6443" ".auth.mydomain.no" }}'
            - name: REFRESH_INTERVAL
              value: "300"   # seconds — poll API server for key rotations
          readinessProbe:
            httpGet:
              path: /.well-known/openid-configuration
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8080
            initialDelaySeconds: 10
            periodSeconds: 30
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 200m
              memory: 128Mi
```

> **Note on hub templates:** The `{{ ... }}` syntax above is ACM hub-side policy templating. It allows you to dynamically derive the cluster-specific issuer URL from the cluster's Infrastructure CR. This means you write ONE manifest that works for ALL clusters. If your proxy gets the issuer from a different mechanism, adjust accordingly.

### manifests/06-service.yaml

```yaml
apiVersion: v1
kind: Service
metadata:
  name: oidc-proxy
  namespace: openshift-oidc-proxy
spec:
  selector:
    app: oidc-proxy
  ports:
    - port: 8080
      targetPort: 8080
      name: http
```

### manifests/07-route.yaml

```yaml
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: oidc-proxy
  namespace: openshift-oidc-proxy
  annotations:
    # Tells cert-manager or your cert controller to manage TLS
    cert-manager.io/issuer-name: letsencrypt-prod
    cert-manager.io/issuer-kind: ClusterIssuer
spec:
  # Host is dynamically derived per cluster using ACM hub templates
  host: '{{ (lookup "config.openshift.io/v1" "Infrastructure" "" "cluster").status.apiServerURL | replace "https://api." "openshift-oidc-" | replace ":6443" ".auth.mydomain.no" }}'
  to:
    kind: Service
    name: oidc-proxy
    weight: 100
  port:
    targetPort: http
  tls:
    termination: edge
    insecureEdgeTerminationPolicy: Redirect
    # If NOT using cert-manager, specify the cert/key inline or via a Secret reference.
    # If using cert-manager with the annotation above, cert-manager handles this.
```

### manifests/08-authentication-cr.yaml

```yaml
# Changes the cluster's service account issuer
# WARNING: This causes API server pods to restart and invalidates existing tokens
apiVersion: config.openshift.io/v1
kind: Authentication
metadata:
  name: cluster
spec:
  serviceAccountIssuer: '{{ (lookup "config.openshift.io/v1" "Infrastructure" "" "cluster").status.apiServerURL | replace "https://api." "https://openshift-oidc-" | replace ":6443" ".auth.mydomain.no" }}'
```

### manifests/09-cert-manager-certificate.yaml

```yaml
# cert-manager Certificate CR — requests and auto-renews the TLS cert
# Requires cert-manager to be installed on the cluster (deploy via a separate ACM policy)
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: oidc-proxy-cert
  namespace: openshift-oidc-proxy
spec:
  secretName: oidc-proxy-tls
  issuerRef:
    name: letsencrypt-prod          # Or your internal CA ClusterIssuer
    kind: ClusterIssuer
  dnsNames:
    - '{{ (lookup "config.openshift.io/v1" "Infrastructure" "" "cluster").status.apiServerURL | replace "https://api." "openshift-oidc-" | replace ":6443" ".auth.mydomain.no" }}'
  duration: 2160h                   # 90 days
  renewBefore: 720h                 # Renew 30 days before expiry
  privateKey:
    algorithm: RSA
    size: 2048
```

---

## Step 3: Certificate Expiry Monitoring with CertificatePolicy

ACM has a built-in **Certificate Policy Controller** that monitors TLS certificates in Secrets across namespaces and raises compliance violations when certificates approach expiration. This is critical — if the OIDC proxy certificate expires, all workload identity federation breaks silently.

### manifests/10-certificate-policy.yaml

```yaml
# CertificatePolicy — monitors certificates in the oidc-proxy namespace
# This is an ACM-native policy template (not a raw K8s manifest)
# The PolicyGenerator wraps it as a policy-template directly
apiVersion: policy.open-cluster-management.io/v1
kind: CertificatePolicy
metadata:
  name: oidc-proxy-cert-expiry
spec:
  # Namespaces to monitor for certificate Secrets
  namespaceSelector:
    include:
      - openshift-oidc-proxy
  # Alert when certificate is within 720 hours (30 days) of expiry
  minimumDuration: 720h
  # Optional: Maximum allowed certificate duration (enforce short-lived certs)
  maximumDuration: 8760h            # 365 days max
  # Optional: Maximum CA certificate duration
  maximumCADuration: 87600h         # 10 years
  # Optional: Disallow wildcard SANs for security
  disallowedSANPattern: "[\\*]"
  # This is inform-only — you cannot auto-renew a cert via remediation
  remediationAction: inform
  severity: high
```

**How this works in practice:**

1. The CertificatePolicy controller runs on each managed cluster
2. It scans all Secrets of type `kubernetes.io/tls` in the `openshift-oidc-proxy` namespace
3. If any certificate's `Not After` date is within 720 hours (30 days), it reports **NonCompliant**
4. The violation propagates to the ACM hub, where it appears in the Governance dashboard
5. If you have alerting configured on ACM (via Grafana/Prometheus on the hub), you get notified

**Why `inform` not `enforce`:** The CertificatePolicy controller can detect expiry but cannot renew certificates. Renewal is handled by cert-manager (if used) or your manual process. The policy's job is to alert you if renewal fails for any reason.

### Additional monitoring layers

For defense in depth, also consider:

```yaml
# ConfigurationPolicy to verify cert-manager Certificate CR status is "Ready"
apiVersion: policy.open-cluster-management.io/v1
kind: ConfigurationPolicy
metadata:
  name: oidc-cert-ready-check
spec:
  remediationAction: inform
  severity: high
  namespaceSelector:
    include:
      - openshift-oidc-proxy
  object-templates:
    - complianceType: musthave
      objectDefinition:
        apiVersion: cert-manager.io/v1
        kind: Certificate
        metadata:
          name: oidc-proxy-cert
          namespace: openshift-oidc-proxy
        status:
          conditions:
            - type: Ready
              status: "True"
```

This catches situations where cert-manager exists but the Certificate is in a failed state (e.g., DNS challenge failure, issuer misconfigured). Combined with the CertificatePolicy, you have two independent checks.

---

## Step 4: PolicyGenerator Configuration

This is the core file that ties everything together. The PolicyGenerator is a Kustomize plugin that takes raw manifests and wraps them into ACM Policy, Placement, and PlacementBinding resources.

### policy-generator-config.yaml

```yaml
apiVersion: policy.open-cluster-management.io/v1
kind: PolicyGenerator
metadata:
  name: oidc-workload-identity

# Placement binding defaults
placementBindingDefaults:
  name: binding-oidc-workload-identity

# Policy defaults — apply to all policies unless overridden
policyDefaults:
  namespace: oidc-policies
  remediationAction: enforce
  severity: medium
  complianceType: musthave
  # Use the Placement API (NOT PlacementRule)
  placement:
    placementPath: placement.yaml             # References our Placement CR
  # Group all policies into a PolicySet
  policySets:
    - oidc-workload-identity
  # Evaluation intervals
  evaluationInterval:
    compliant: 30m                            # Re-check every 30 min when compliant
    noncompliant: 5m                          # Re-check every 5 min when non-compliant
  # Enable hub-side templating for dynamic cluster values
  configurationPolicyAnnotations:
    policy.open-cluster-management.io/trigger-update: "1"

# ────────────────────────────────────────────────────────────────
# Policy definitions
# ────────────────────────────────────────────────────────────────
policies:

  # --- Policy 1: Namespace ---
  - name: oidc-proxy-namespace
    manifests:
      - path: manifests/01-namespace.yaml

  # --- Policy 2: RBAC (ServiceAccount + ClusterRole + Binding) ---
  - name: oidc-proxy-rbac
    manifests:
      - path: manifests/02-serviceaccount.yaml
      - path: manifests/03-clusterrole.yaml
      - path: manifests/04-clusterrolebinding.yaml

  # --- Policy 3: Deployment + Service ---
  - name: oidc-proxy-deployment
    manifests:
      - path: manifests/05-deployment.yaml
      - path: manifests/06-service.yaml

  # --- Policy 4: Route ---
  - name: oidc-proxy-route
    manifests:
      - path: manifests/07-route.yaml

  # --- Policy 5: Authentication CR (issuer change) ---
  - name: oidc-issuer-config
    severity: high                            # Override — this is critical
    manifests:
      - path: manifests/08-authentication-cr.yaml
        complianceType: musthave

  # --- Policy 6: cert-manager Certificate ---
  - name: oidc-cert-manager
    manifests:
      - path: manifests/09-cert-manager-certificate.yaml

  # --- Policy 7: Certificate expiry monitoring ---
  - name: oidc-certificate-monitoring
    remediationAction: inform                 # Override — cannot enforce cert renewal
    severity: high
    manifests:
      - path: manifests/10-certificate-policy.yaml

# ────────────────────────────────────────────────────────────────
# PolicySet definition
# ────────────────────────────────────────────────────────────────
policySets:
  - name: oidc-workload-identity
    description: >-
      Deploys and enforces the OIDC proxy infrastructure required for
      Azure Entra workload identity federation on all labeled clusters.
    policies:
      - oidc-proxy-namespace
      - oidc-proxy-rbac
      - oidc-proxy-deployment
      - oidc-proxy-route
      - oidc-issuer-config
      - oidc-cert-manager
      - oidc-certificate-monitoring
```

### kustomization.yaml

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

generators:
  - policy-generator-config.yaml

resources:
  - managed-cluster-set-binding.yaml
```

---

## Step 5: What the PolicyGenerator Produces

When you run `kustomize build --enable-alpha-plugins .`, the PolicyGenerator outputs the following resources. You don't write these by hand — the generator creates them:

```
┌────────────────────────────────────────────────────────────────────────────────┐
│  GENERATED BY PolicyGenerator (you never write these manually)                 │
│                                                                                │
│  1. Placement         (from placement.yaml — passed through)                   │
│     apiVersion: cluster.open-cluster-management.io/v1beta1    ◄── CURRENT API  │
│     kind: Placement                                                            │
│     NOT: apps.open-cluster-management.io/v1 PlacementRule     ◄── DEPRECATED   │
│                                                                                │
│  2. PlacementBinding  (auto-generated)                                         │
│     placementRef:                                                              │
│       kind: Placement                                                          │
│       apiGroup: cluster.open-cluster-management.io            ◄── CURRENT API  │
│     subjects:                                                                  │
│       - kind: PolicySet                                                        │
│         name: oidc-workload-identity                                           │
│                                                                                │
│  3. PolicySet          (auto-generated from policySets section)                 │
│                                                                                │
│  4. Policy x7          (auto-generated, each wrapping ConfigurationPolicy      │
│                         or CertificatePolicy templates)                        │
│                                                                                │
└────────────────────────────────────────────────────────────────────────────────┘
```

---

## Step 6: Deploying via GitOps (ArgoCD / OpenShift GitOps)

The recommended deployment model is to have ArgoCD on the hub cluster watch the Git repository and sync the generated policies.

### ArgoCD Application

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: oidc-workload-identity-policies
  namespace: openshift-gitops
spec:
  project: default
  source:
    repoURL: https://git.mydomain.no/platform/acm-oidc-policies.git
    targetRevision: main
    path: .
    # CRITICAL: Enable the PolicyGenerator Kustomize plugin
    plugin:
      name: kustomize
      env:
        - name: KUSTOMIZE_BUILD_OPTIONS
          value: "--enable-alpha-plugins"
  destination:
    server: https://kubernetes.default.svc
    namespace: oidc-policies
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

Alternatively, if using ACM's built-in **Subscription** GitOps model (Application Lifecycle), the Kustomize PolicyGenerator plugin is natively supported and will be executed automatically.

---

## Step 7: Onboarding a New Cluster

With the hybrid ACM + Ansible approach fully wired (see Steps 8–13), onboarding a new cluster is a **single-step process**:

### Label the cluster in ACM

```bash
# Option 1: CLI
oc label managedcluster <cluster-name> oidc-federation=enabled --overwrite

# Option 2: ACM Console
# Navigate to Infrastructure > Clusters > <cluster> > Labels
# Add: oidc-federation=enabled
```

**Everything cascades automatically from this single label:**

1. The Placement evaluates and includes the new cluster
2. ACM propagates all 8 policies to the cluster (in-cluster resources + external infra check)
3. Policies 1–7 deploy the OIDC proxy, Route, RBAC, issuer config, and cert-manager Certificate
4. Policy 8 (external-infra-check) goes NonCompliant because the ConfigMap doesn't exist yet
5. The `PolicyAutomation` triggers an Ansible job in AAP Controller
6. Ansible configures F5 BIG-IP, DNS, and Azure Entra, then creates the ConfigMap on the cluster
7. Policy 8 becomes Compliant — all 8 policies are green

No Terraform, no manual pipeline, no second step. See **Steps 8–13** below for the full implementation details of the ACM → Ansible integration.

---

## Migrating from PlacementRule to Placement

If you have existing policies using the deprecated `PlacementRule`, here is the migration path:

### Before (DEPRECATED — do not use)

```yaml
# ❌ DEPRECATED
apiVersion: apps.open-cluster-management.io/v1
kind: PlacementRule
metadata:
  name: placement-oidc
  namespace: oidc-policies
spec:
  clusterConditions:
    - status: "True"
      type: ManagedClusterConditionAvailable
  clusterSelector:
    matchExpressions:
      - key: oidc-federation
        operator: In
        values:
          - "enabled"
---
# ❌ DEPRECATED binding style
apiVersion: policy.open-cluster-management.io/v1
kind: PlacementBinding
metadata:
  name: binding-oidc
  namespace: oidc-policies
placementRef:
  apiGroup: apps.open-cluster-management.io      # ← Old API group
  kind: PlacementRule                             # ← Deprecated kind
  name: placement-oidc
subjects:
  - apiGroup: policy.open-cluster-management.io
    kind: Policy
    name: oidc-proxy-namespace
```

### After (CURRENT — use this)

```yaml
# ✅ CURRENT — ManagedClusterSetBinding (new prerequisite)
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSetBinding
metadata:
  name: all-openshift-clusters
  namespace: oidc-policies
spec:
  clusterSet: all-openshift-clusters
---
# ✅ CURRENT — Placement
apiVersion: cluster.open-cluster-management.io/v1beta1
kind: Placement
metadata:
  name: placement-oidc
  namespace: oidc-policies
spec:
  clusterSets:
    - all-openshift-clusters
  predicates:
    - requiredClusterSelector:
        labelSelector:
          matchExpressions:
            - key: oidc-federation
              operator: In
              values:
                - "enabled"
---
# ✅ CURRENT — PlacementBinding with updated apiGroup
apiVersion: policy.open-cluster-management.io/v1
kind: PlacementBinding
metadata:
  name: binding-oidc
  namespace: oidc-policies
placementRef:
  apiGroup: cluster.open-cluster-management.io    # ← New API group
  kind: Placement                                 # ← Current kind
  name: placement-oidc
subjects:
  - apiGroup: policy.open-cluster-management.io
    kind: PolicySet
    name: oidc-workload-identity
```

**Key differences to note:**


| Aspect         | PlacementRule (deprecated)              | Placement (current)                                       |
| -------------- | --------------------------------------- | --------------------------------------------------------- |
| API Group      | `apps.open-cluster-management.io/v1`    | `cluster.open-cluster-management.io/v1beta1`              |
| Cluster access | Unrestricted — could select any cluster | Requires `ManagedClusterSetBinding`                       |
| Selector path  | `spec.clusterSelector`                  | `spec.predicates[].requiredClusterSelector.labelSelector` |
| Cluster health | `spec.clusterConditions`                | Built-in (no separate field needed)                       |
| RBAC           | No namespace scoping                    | Scoped via `ManagedClusterSet` binding                    |


---

## Compliance Dashboard View

Once deployed, the ACM Governance dashboard will show:

```
┌─────────────────────────────────────────────────────────────────────────┐
│  PolicySet: oidc-workload-identity                                      │
│  Status: 2 of 3 clusters compliant                                      │
│                                                                         │
│  ┌────────────────────────────────┬──────────┬──────────┬──────────┐    │
│  │ Policy                         │ Cluster1 │ Cluster2 │ Cluster3 │    │
│  ├────────────────────────────────┼──────────┼──────────┼──────────┤    │
│  │ oidc-proxy-namespace           │ ✅       │ ✅       │ ✅       │    │
│  │ oidc-proxy-rbac                │ ✅       │ ✅       │ ✅       │    │
│  │ oidc-proxy-deployment          │ ✅       │ ✅       │ ✅       │    │
│  │ oidc-proxy-route               │ ✅       │ ✅       │ ✅       │    │
│  │ oidc-issuer-config             │ ✅       │ ✅       │ ✅       │    │
│  │ oidc-cert-manager              │ ✅       │ ✅       │ ✅       │    │
│  │ oidc-certificate-monitoring    │ ✅       │ ✅       │ ⚠️ 25d   │    │
│  │                                │          │          │  to exp  │    │
│  └────────────────────────────────┴──────────┴──────────┴──────────┘    │
│                                                                         │
│  ⚠️ Cluster3: Certificate in openshift-oidc-proxy/oidc-proxy-tls       │
│     expires in 25 days — investigate cert-manager renewal               │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## The Hybrid Approach: ACM + Ansible Automation Platform

The full onboarding workflow spans two domains: in-cluster resources (managed by ACM policies) and external infrastructure — F5 BIG-IP, DNS, and Azure Entra — which ACM cannot manage natively. The hybrid approach uses Ansible Automation Platform (AAP) for the external pieces, and the key insight is that **ACM can trigger Ansible directly** through the `PolicyAutomation` resource and the `ClusterCurator` resource, giving you a fully integrated, event-driven pipeline.

### Three Ways ACM Can Trigger Ansible

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                  ACM ←→ ANSIBLE INTEGRATION POINTS                           │
│                                                                              │
│  ┌────────────────────────────────────────────────────────────────────────┐   │
│  │  1. PolicyAutomation (Governance)                                     │   │
│  │     Trigger: Policy goes NonCompliant                                 │   │
│  │     Use case: Certificate expiry → open ServiceNow ticket             │   │
│  │              OIDC proxy down → trigger remediation playbook           │   │
│  │     Modes: once | everyEvent | disabled                               │   │
│  │     CR: policy.open-cluster-management.io/v1beta1/PolicyAutomation    │   │
│  └────────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│  ┌────────────────────────────────────────────────────────────────────────┐   │
│  │  2. ClusterCurator (Cluster Lifecycle)                                │   │
│  │     Trigger: Cluster create / upgrade / destroy events                │   │
│  │     Use case: Post-install → configure F5 + DNS + Entra              │   │
│  │              Pre-upgrade → drain F5 pool member                       │   │
│  │     Hooks: prehook / posthook per lifecycle stage                     │   │
│  │     CR: cluster.open-cluster-management.io/v1beta1/ClusterCurator    │   │
│  └────────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│  ┌────────────────────────────────────────────────────────────────────────┐   │
│  │  3. AnsibleJob (Direct / Subscription hooks)                          │   │
│  │     Trigger: Manual, or prehook/posthook in Git subscriptions         │   │
│  │     Use case: Ad-hoc automation, app deployment hooks                 │   │
│  │     CR: tower.ansible.com/v1alpha1/AnsibleJob                        │   │
│  └────────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│  All three require:                                                          │
│  • Ansible Automation Platform Resource Operator installed on hub            │
│  • A Secret with AAP/Tower credentials (token + host URL)                   │
│  • Job Templates pre-configured in AAP/Controller                           │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## Step 8: Prerequisites — Ansible Automation Platform Integration

### 8a. Install the Ansible Automation Platform Resource Operator

On the ACM hub cluster, install the **Ansible Automation Platform Resource Operator** from OperatorHub. This operator watches for `AnsibleJob` CRs and triggers the corresponding job templates in your AAP Controller.

### 8b. Create the AAP Credential Secret

The secret must be in the **same namespace** as your policies (where the PolicyAutomation CR will live):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aap-credentials
  namespace: oidc-policies
type: Opaque
stringData:
  token: "<your-aap-controller-api-token>"
  host: "https://aap-controller.mydomain.no"
```

### 8c. Create Job Templates in AAP Controller

You need three job templates in your Ansible Automation Platform Controller:


| Job Template Name     | Purpose                                               | Playbook                               |
| --------------------- | ----------------------------------------------------- | -------------------------------------- |
| `OIDC-Configure-F5`   | Create BIG-IP VIP, pool, health monitor, TLS profile  | `playbooks/f5-oidc-setup.yml`          |
| `OIDC-Configure-DNS`  | Create DNS A record pointing to F5 VIP                | `playbooks/dns-oidc-setup.yml`         |
| `OIDC-Register-Entra` | Register federated identity credential in Azure Entra | `playbooks/entra-federation-setup.yml` |


Or combine them into a single workflow template: `OIDC-External-Infra-Onboarding`.

---

## Step 9: Triggering Ansible from ACM — Option A: ClusterCurator (Cluster Lifecycle)

If your clusters are **provisioned through ACM** (via Hive/ClusterDeployment), the `ClusterCurator` is the most natural integration point. It runs Ansible jobs as prehook/posthook steps during cluster install, upgrade, or scale operations.

### ClusterCurator Template

```yaml
# This template is assigned to clusters during provisioning in the ACM console
# or can be applied programmatically
apiVersion: cluster.open-cluster-management.io/v1beta1
kind: ClusterCurator
metadata:
  name: oidc-onboarding-template
  namespace: <cluster-namespace>             # Same namespace as the ClusterDeployment
spec:
  install:
    # Jobs to run AFTER the cluster is successfully installed
    towerAuthSecret: aap-credentials
    posthook:
      # Step 1: Configure F5 load balancer
      - name: OIDC-Configure-F5
        extra_vars:
          cluster_name: "{{ .ClusterDeployment.metadata.name }}"
          vip_address: "10.0.100.50"
          pool_member_port: 443
          health_monitor_path: "/.well-known/openid-configuration"

      # Step 2: Create DNS record
      - name: OIDC-Configure-DNS
        extra_vars:
          cluster_name: "{{ .ClusterDeployment.metadata.name }}"
          dns_zone: "auth.mydomain.no"
          record_name: "openshift-oidc-{{ .ClusterDeployment.metadata.name }}"
          record_type: "A"
          target_ip: "10.0.100.50"

      # Step 3: Register in Azure Entra
      - name: OIDC-Register-Entra
        extra_vars:
          cluster_name: "{{ .ClusterDeployment.metadata.name }}"
          issuer_url: "https://openshift-oidc-{{ .ClusterDeployment.metadata.name }}.auth.mydomain.no"
          azure_app_id: "<app-registration-client-id>"
          azure_tenant_id: "<tenant-id>"

  upgrade:
    # Optional: prehook to drain F5 pool before upgrade
    towerAuthSecret: aap-credentials
    prehook:
      - name: OIDC-F5-Drain-Pool
        extra_vars:
          cluster_name: "{{ .ClusterDeployment.metadata.name }}"
          action: "drain"
    posthook:
      - name: OIDC-F5-Enable-Pool
        extra_vars:
          cluster_name: "{{ .ClusterDeployment.metadata.name }}"
          action: "enable"
```

**How this works in the flow:**

```
  ACM provisions cluster    Cluster install completes     ACM runs posthook Ansible jobs
         │                           │                              │
         ▼                           ▼                              ▼
  ┌──────────────┐          ┌────────────────┐          ┌───────────────────────┐
  │ ClusterDeploy│          │ Cluster Ready  │          │ 1. F5 VIP created     │
  │ ment created │─────────►│ ACM policies   │─────────►│ 2. DNS record created │
  │ + Curator    │          │ start applying │          │ 3. Entra registered   │
  └──────────────┘          └────────────────┘          └───────────────────────┘
                                    │
                                    ▼
                            ┌────────────────┐
                            │ OIDC proxy     │
                            │ deployed by    │
                            │ ACM policy     │
                            │ (in parallel)  │
                            └────────────────┘
```

> **Important:** ClusterCurator posthooks run after the cluster is installed and ready, but the ACM policies (which deploy the OIDC proxy) may still be propagating. The F5 health monitor will show the pool member as down until the OIDC proxy pod is running and the Route is active. This is fine — the F5 will start routing traffic once the health check passes.

---

## Step 10: Triggering Ansible from ACM — Option B: PolicyAutomation (Governance Events)

If your clusters are **not provisioned by ACM** (e.g., pre-existing baremetal clusters imported into ACM), or if you want event-driven automation that responds to policy compliance state changes, use `PolicyAutomation`.

### How PolicyAutomation Works

```
  ┌─────────────┐     Policy goes      ┌──────────────────┐     Creates     ┌─────────────┐
  │  ACM Policy  │──── NonCompliant ───►│ PolicyAutomation  │───────────────►│  AnsibleJob  │
  │  on hub      │                      │  (watches policy) │                │  CR on hub   │
  └─────────────┘                       └──────────────────┘                └──────┬──────┘
                                                                                   │
                                                                    AAP Resource   │
                                                                    Operator       │
                                                                    picks it up    │
                                                                                   ▼
                                                                          ┌─────────────────┐
                                                                          │ AAP Controller   │
                                                                          │ runs job template│
                                                                          │ with extra_vars  │
                                                                          │ including         │
                                                                          │ target_clusters  │
                                                                          └─────────────────┘
```

### PolicyAutomation Modes


| Mode         | Behavior                                                                                                                                                                                                                       |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `once`       | Runs the Ansible job the first time the policy is NonCompliant, then automatically sets itself to `disabled`. An admin must manually re-enable it by setting the annotation `policy.open-cluster-management.io/rerun: "true"`. |
| `everyEvent` | Runs the Ansible job every time the policy transitions to NonCompliant. Use `delayAfterRunSeconds` to prevent rapid-fire triggering.                                                                                           |
| `disabled`   | PolicyAutomation exists but does not trigger. Useful for pausing.                                                                                                                                                              |


### PolicyAutomation for External Infra Onboarding

Create a "canary" policy that checks whether the external infrastructure exists. When it's NonCompliant (meaning external infra is missing), it triggers the Ansible workflow:

#### manifests/11-external-infra-check.yaml

```yaml
# ConfigurationPolicy that checks for a ConfigMap indicating external infra is configured
# This ConfigMap is created by the Ansible playbook after successful setup
apiVersion: policy.open-cluster-management.io/v1
kind: ConfigurationPolicy
metadata:
  name: oidc-external-infra-status
spec:
  remediationAction: inform              # Inform only — we want to detect, not fix
  severity: high
  namespaceSelector:
    include:
      - openshift-oidc-proxy
  object-templates:
    - complianceType: musthave
      objectDefinition:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: oidc-external-infra-ready
          namespace: openshift-oidc-proxy
        data:
          f5_configured: "true"
          dns_configured: "true"
          entra_configured: "true"
```

Add this as Policy 8 in the PolicyGenerator:

```yaml
  # --- Policy 8: External infra readiness check (inform only) ---
  - name: oidc-external-infra-check
    remediationAction: inform
    severity: high
    manifests:
      - path: manifests/11-external-infra-check.yaml
```

#### PolicyAutomation CR

```yaml
# When oidc-external-infra-check is NonCompliant (ConfigMap missing),
# trigger the Ansible workflow to configure F5 + DNS + Entra
apiVersion: policy.open-cluster-management.io/v1beta1
kind: PolicyAutomation
metadata:
  name: oidc-external-infra-automation
  namespace: oidc-policies
  # To manually re-trigger after "once" mode has fired:
  # annotations:
  #   policy.open-cluster-management.io/rerun: "true"
spec:
  policyRef: oidc-external-infra-check        # References the policy name
  mode: everyEvent                            # Or "once" for single trigger
  automationDef:
    name: OIDC-External-Infra-Onboarding      # Job Template name in AAP
    secret: aap-credentials                   # Secret with AAP token + host
    type: AnsibleJob
    extra_vars:
      # ACM automatically injects target_clusters with the list of
      # non-compliant cluster names. The Ansible playbook uses this
      # to know which cluster(s) need external infra setup.
      dns_zone: "auth.mydomain.no"
      azure_tenant_id: "<tenant-id>"
      azure_app_id: "<app-registration-client-id>"
    # Prevent rapid re-triggering
    delayAfterRunSeconds: 300                 # Wait 5 min between runs
```

### The Ansible Playbook (AAP Side)

The Ansible playbook that ACM triggers receives `target_clusters` (an array of non-compliant cluster names) and `extra_vars` as input. Here's the structure:

```
ansible-oidc-infra/
├── playbooks/
│   ├── oidc-full-onboarding.yml          # Workflow: calls all three below
│   ├── f5-oidc-setup.yml                 # F5 BIG-IP configuration
│   ├── dns-oidc-setup.yml                # DNS record creation
│   ├── entra-federation-setup.yml        # Azure Entra registration
│   └── mark-infra-ready.yml             # Creates the ConfigMap on cluster
│
├── roles/
│   ├── f5_virtual_server/
│   │   └── tasks/main.yml
│   ├── dns_record/
│   │   └── tasks/main.yml
│   ├── azure_entra_federation/
│   │   └── tasks/main.yml
│   └── k8s_infra_status/
│       └── tasks/main.yml
│
├── inventory/
│   └── hosts.yml
│
└── group_vars/
    └── all.yml
```

```yaml
playbooks/oidc-full-onboarding.yml
```

```yaml
---
# This playbook is triggered by ACM PolicyAutomation
# ACM passes 'target_clusters' as a list of non-compliant cluster names
- name: Configure external infrastructure for OIDC workload identity
  hosts: localhost
  connection: local
  gather_facts: false

  vars:
    domain: "auth.mydomain.no"

  tasks:
    - name: Process each non-compliant cluster
      include_tasks: per-cluster-setup.yml
      loop: "{{ target_clusters }}"
      loop_control:
        loop_var: cluster_name

---
# per-cluster-setup.yml (included per cluster)
- name: Set derived variables
  set_fact:
    oidc_hostname: "openshift-oidc-{{ cluster_name }}.{{ domain }}"
    issuer_url: "https://openshift-oidc-{{ cluster_name }}.{{ domain }}"

# ────── F5 BIG-IP ──────
- name: Configure F5 BIG-IP virtual server
  include_role:
    name: f5_virtual_server
  vars:
    vs_name: "vs_oidc_{{ cluster_name }}"
    vs_destination: "{{ f5_vip_address }}:443"
    pool_name: "pool_oidc_{{ cluster_name }}"
    pool_members:
      - host: "{{ cluster_router_ip }}"
        port: 443
    health_monitor:
      type: https
      send: "GET /.well-known/openid-configuration HTTP/1.1\\r\\nHost: {{ oidc_hostname }}\\r\\n\\r\\n"
      receive: "jwks_uri"
    ssl_profile:
      client: "clientssl_oidc_{{ cluster_name }}"
      cert: "/Common/{{ oidc_hostname }}.crt"
      key: "/Common/{{ oidc_hostname }}.key"

# ────── DNS ──────
- name: Create DNS record
  include_role:
    name: dns_record
  vars:
    record_name: "openshift-oidc-{{ cluster_name }}"
    zone: "{{ domain }}"
    record_type: A
    record_value: "{{ f5_vip_address }}"

# ────── Azure Entra ──────
- name: Register federated identity credential in Azure Entra
  include_role:
    name: azure_entra_federation
  vars:
    app_object_id: "{{ azure_app_object_id }}"
    credential_name: "oidc-{{ cluster_name }}"
    issuer: "{{ issuer_url }}"
    subject: "system:serviceaccount:{{ workload_namespace }}:{{ workload_sa }}"
    audience: "api://AzureADTokenExchange"

# ────── Mark complete ──────
- name: Create ConfigMap on cluster to signal readiness
  include_role:
    name: k8s_infra_status
  vars:
    configmap_name: oidc-external-infra-ready
    configmap_namespace: openshift-oidc-proxy
    target_cluster: "{{ cluster_name }}"
    data:
      f5_configured: "true"
      dns_configured: "true"
      entra_configured: "true"
      configured_at: "{{ ansible_date_time.iso8601 }}"
```

#### roles/f5_virtual_server/tasks/main.yml (F5 BIG-IP)

```yaml
---
# Uses the F5 Ansible collection: f5networks.f5_modules
- name: Create F5 pool
  f5networks.f5_modules.bigip_pool:
    name: "{{ pool_name }}"
    lb_method: round-robin
    monitors:
      - /Common/https_oidc_{{ cluster_name }}
    provider:
      server: "{{ f5_host }}"
      user: "{{ f5_user }}"
      password: "{{ f5_password }}"
      validate_certs: true

- name: Add pool members
  f5networks.f5_modules.bigip_pool_member:
    pool: "{{ pool_name }}"
    host: "{{ item.host }}"
    port: "{{ item.port }}"
    provider:
      server: "{{ f5_host }}"
      user: "{{ f5_user }}"
      password: "{{ f5_password }}"
      validate_certs: true
  loop: "{{ pool_members }}"

- name: Create HTTPS health monitor
  f5networks.f5_modules.bigip_monitor_https:
    name: "https_oidc_{{ cluster_name }}"
    send: "{{ health_monitor.send }}"
    receive: "{{ health_monitor.receive }}"
    interval: 30
    timeout: 91
    provider:
      server: "{{ f5_host }}"
      user: "{{ f5_user }}"
      password: "{{ f5_password }}"
      validate_certs: true

- name: Create virtual server
  f5networks.f5_modules.bigip_virtual_server:
    name: "{{ vs_name }}"
    destination: "{{ vs_destination }}"
    pool: "{{ pool_name }}"
    snat: automap
    profiles:
      - name: "{{ ssl_profile.client }}"
        context: client-side
      - http
    provider:
      server: "{{ f5_host }}"
      user: "{{ f5_user }}"
      password: "{{ f5_password }}"
      validate_certs: true
```

#### roles/azure_entra_federation/tasks/main.yml (Azure Entra)

```yaml
---
# Uses the Azure Ansible collection: azure.azcollection
# Requires: azure-identity, azure-mgmt-authorization pip packages
- name: Register federated identity credential
  azure.azcollection.azure_rm_resource:
    api_version: "2022-01-31-preview"
    resource_group: "{{ azure_resource_group }}"
    provider: "Microsoft.ManagedIdentity"
    resource_type: "userAssignedIdentities"
    resource_name: "{{ azure_managed_identity_name }}"
    subresource:
      - type: "federatedIdentityCredentials"
        name: "{{ credential_name }}"
    body:
      properties:
        issuer: "{{ issuer }}"
        subject: "{{ subject }}"
        audiences:
          - "{{ audience }}"
    state: present

# Alternative using the az CLI module if azure.azcollection doesn't fit:
# - name: Register federated identity credential (CLI)
#   ansible.builtin.command: >
#     az identity federated-credential create
#       --name "{{ credential_name }}"
#       --identity-name "{{ azure_managed_identity_name }}"
#       --resource-group "{{ azure_resource_group }}"
#       --issuer "{{ issuer }}"
#       --subject "{{ subject }}"
#       --audiences "{{ audience }}"
```

---

## Step 11: The Complete Feedback Loop

The critical design element is the **ConfigMap feedback loop**. After the Ansible playbook configures external infrastructure, it creates a ConfigMap (`oidc-external-infra-ready`) on the target cluster. The ACM policy that was NonCompliant (triggering the automation) now becomes Compliant, which means the PolicyAutomation stops firing.

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                    SELF-HEALING FEEDBACK LOOP                                 │
│                                                                              │
│  ┌─────────┐    label added    ┌──────────┐    policies     ┌────────────┐  │
│  │ New      │─────────────────►│ ACM Hub  │───propagated───►│ Managed    │  │
│  │ Cluster  │                  │          │                 │ Cluster    │  │
│  │ imported │                  └─────┬────┘                 └─────┬──────┘  │
│  └─────────┘                        │                             │         │
│                                     │                             │         │
│                          Policy 8 goes                   Policies 1-7      │
│                          NonCompliant                    start deploying    │
│                          (ConfigMap missing)             OIDC proxy         │
│                                     │                             │         │
│                                     ▼                             │         │
│                          ┌──────────────────┐                     │         │
│                          │ PolicyAutomation  │                     │         │
│                          │ triggers Ansible  │                     │         │
│                          └────────┬─────────┘                     │         │
│                                   │                               │         │
│                                   ▼                               │         │
│                          ┌──────────────────┐                     │         │
│                          │ AAP Controller   │                     │         │
│                          │ runs workflow:   │                     │         │
│                          │  1. F5 setup     │                     │         │
│                          │  2. DNS record   │                     │         │
│                          │  3. Entra reg    │                     │         │
│                          │  4. ConfigMap ───┼─── creates on ──────┤         │
│                          │     created      │    managed cluster  │         │
│                          └──────────────────┘                     │         │
│                                                                   │         │
│                                                          ConfigMap exists   │
│                                                          Policy 8 → ✅      │
│                                                          Compliant          │
│                                                                             │
│                                                          PolicyAutomation   │
│                                                          stops triggering   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Step 12: Updated Git Repository Structure (Hybrid)

```
acm-oidc-policies/
├── README.md
├── kustomization.yaml
├── policy-generator-config.yaml
├── placement.yaml
├── managed-cluster-set-binding.yaml
├── policy-automation.yaml                      # PolicyAutomation CR (NEW)
│
├── manifests/
│   ├── 01-namespace.yaml
│   ├── 02-serviceaccount.yaml
│   ├── 03-clusterrole.yaml
│   ├── 04-clusterrolebinding.yaml
│   ├── 05-deployment.yaml
│   ├── 06-service.yaml
│   ├── 07-route.yaml
│   ├── 08-authentication-cr.yaml
│   ├── 09-cert-manager-certificate.yaml
│   ├── 10-certificate-policy.yaml
│   └── 11-external-infra-check.yaml           # External infra canary (NEW)
│
└── ansible/                                    # Ansible playbooks (in AAP Git project)
    ├── playbooks/
    │   ├── oidc-full-onboarding.yml
    │   ├── f5-oidc-setup.yml
    │   ├── dns-oidc-setup.yml
    │   ├── entra-federation-setup.yml
    │   └── mark-infra-ready.yml
    ├── roles/
    │   ├── f5_virtual_server/
    │   ├── dns_record/
    │   ├── azure_entra_federation/
    │   └── k8s_infra_status/
    └── group_vars/
        └── all.yml
```

---

## Step 13: Certificate Expiry — Triggering Ansible Remediation

Remember the CertificatePolicy from Step 3? You can attach a **second PolicyAutomation** that triggers an Ansible job when a certificate approaches expiry. This can open a ticket, send a Slack alert, or even attempt automated renewal:

```yaml
apiVersion: policy.open-cluster-management.io/v1beta1
kind: PolicyAutomation
metadata:
  name: oidc-cert-expiry-automation
  namespace: oidc-policies
spec:
  policyRef: oidc-certificate-monitoring
  mode: everyEvent
  automationDef:
    name: OIDC-Certificate-Expiry-Alert       # Job Template in AAP
    secret: aap-credentials
    type: AnsibleJob
    extra_vars:
      alert_channel: "#platform-alerts"
      severity: "high"
      escalation_hours: 48
    delayAfterRunSeconds: 86400               # Once per day max
```

The Ansible job template can:

- Send a Slack/Teams notification
- Open a ServiceNow incident
- Trigger cert-manager renewal if it's stuck
- Force-rotate the certificate via the cluster API
- Update the F5 TLS profile with the new cert if needed

---

## Choosing Between ClusterCurator and PolicyAutomation


| Aspect             | ClusterCurator                                     | PolicyAutomation                               |
| ------------------ | -------------------------------------------------- | ---------------------------------------------- |
| **Trigger**        | Cluster lifecycle events (install, upgrade, scale) | Policy compliance state changes                |
| **Best for**       | Clusters provisioned by ACM (Hive)                 | Pre-existing clusters imported into ACM        |
| **Timing**         | Prehook/posthook around lifecycle events           | Reactive — fires when policy goes NonCompliant |
| **Cluster name**   | Available from ClusterDeployment context           | Passed in `target_clusters` extra_var          |
| **Re-run control** | Automatic per lifecycle event                      | `once` / `everyEvent` / `disabled` modes       |
| **Use for OIDC?**  | Yes — if ACM provisions the clusters               | Yes — if clusters are imported                 |


**For your scenario** (pre-existing baremetal clusters imported into ACM), **PolicyAutomation is the better fit**. ClusterCurator is designed for clusters that ACM provisions via Hive, and your baremetal clusters likely pre-exist.

However, if you ever move to ACM-provisioned clusters, the ClusterCurator posthook approach is cleaner because it fires exactly once at the right moment in the lifecycle, rather than relying on a policy-compliance feedback loop.

---

## Complete End-to-End Architecture (Hybrid)

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              GIT REPOSITORY                                     │
│                                                                                 │
│  ┌───────────────────────┐         ┌──────────────────────────────────────┐      │
│  │ ACM Policy Manifests  │         │ Ansible Playbooks + Roles           │      │
│  │ (K8s YAML)            │         │ (F5, DNS, Entra, notifications)     │      │
│  └───────────┬───────────┘         └───────────────┬──────────────────────┘      │
│              │                                     │                            │
└──────────────┼─────────────────────────────────────┼────────────────────────────┘
               │                                     │
    ArgoCD / ACM Subscription                  AAP Git Project sync
               │                                     │
               ▼                                     ▼
┌──────────────────────────────┐    ┌─────────────────────────────────────────────┐
│         ACM HUB              │    │       ANSIBLE AUTOMATION PLATFORM           │
│                              │    │                                             │
│  PolicyGenerator → Policies  │    │  Job Templates:                             │
│  PolicySet                   │    │    • OIDC-External-Infra-Onboarding         │
│  Placement (not PlacementRule│    │    • OIDC-Certificate-Expiry-Alert          │
│  PolicyAutomation ──────────────────► triggers jobs when policy fires           │
│  CertificatePolicy          │    │    • OIDC-F5-Drain-Pool (pre-upgrade)       │
│                              │    │    • OIDC-F5-Enable-Pool (post-upgrade)     │
│  AAP Resource Operator       │    │                                             │
│  (watches AnsibleJob CRs)  ────────► communicates via API token                │
│                              │    │                                             │
└──────────────┬───────────────┘    └───────────────┬─────────────────────────────┘
               │                                    │
    Policies propagated                    Ansible configures
               │                                    │
     ┌─────────┼──────────┐              ┌──────────┼──────────┐
     ▼         ▼          ▼              ▼          ▼          ▼
┌─────────┐┌─────────┐┌─────────┐  ┌─────────┐┌─────────┐┌──────────┐
│Cluster 1││Cluster 2││Cluster 3│  │F5 BIG-IP││  DNS    ││Azure     │
│         ││         ││         │  │         ││  Server ││Entra ID  │
│ OIDC    ││ OIDC    ││ OIDC    │  │ VIPs    ││ Records ││ Fed.     │
│ Proxy   ││ Proxy   ││ Proxy   │  │ Pools   ││         ││ Creds    │
│ Route   ││ Route   ││ Route   │  │ Monitors││         ││          │
│ Issuer  ││ Issuer  ││ Issuer  │  │ TLS     ││         ││          │
└─────────┘└─────────┘└─────────┘  └─────────┘└─────────┘└──────────┘
     ▲                                  │
     │          ConfigMap feedback       │
     └───────────────────────────────────┘
     Ansible creates ConfigMap on cluster
     → Policy becomes Compliant
     → PolicyAutomation stops firing
```

---

## Summary Checklist: New Cluster Onboarding (Hybrid — Fully Automated)

```
□ 1. Import cluster into ACM hub
□ 2. Ensure cluster is in the correct ManagedClusterSet
□ 3. Label cluster: oidc-federation=enabled
       │
       ├──► ACM auto-deploys all 8 policies to cluster
       │    (namespace, RBAC, deployment, route, issuer config,
       │     cert-manager cert, cert monitoring, external infra check)
       │
       ├──► Policy 8 (external-infra-check) goes NonCompliant
       │    (ConfigMap doesn't exist yet)
       │
       ├──► PolicyAutomation triggers Ansible job in AAP
       │    (passes cluster name via target_clusters)
       │
       ├──► AAP runs workflow:
       │    □ F5 VIP + pool + health monitor created
       │    □ DNS record created
       │    □ Azure Entra federated identity credential registered
       │    □ ConfigMap created on cluster
       │
       ├──► Policy 8 becomes Compliant (ConfigMap exists)
       │    PolicyAutomation stops firing
       │
       └──► All 8 policies green in Governance dashboard

□ 4. Validate (automated or manual):
       □ curl https://openshift-oidc-<cluster>.auth.mydomain.no/.well-known/openid-configuration
       □ Test token exchange with Entra end-to-end
       □ ACM Governance dashboard shows full compliance
```

**The only manual step is adding the label.** Everything else cascades automatically.