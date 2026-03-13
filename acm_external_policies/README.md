# ACM OIDC Workload Identity Policies

Automates the deployment of OIDC proxy infrastructure for Azure Entra workload
identity federation across managed OpenShift clusters, using ACM PolicyGenerator
and Ansible Automation Platform.

## Repository Structure

```
acm-oidc-policies/
├── kustomization.yaml                 # Root kustomization (generators + resources)
├── policy-generator-config.yaml       # PolicyGenerator — wraps manifests into ACM policies
├── placement.yaml                     # Placement (cluster.open-cluster-management.io/v1beta1)
├── managed-cluster-set-binding.yaml   # Binds ManagedClusterSet to policy namespace
├── policy-automation.yaml             # PolicyAutomation — triggers Ansible on NonCompliant
├── policy-automation-cert-expiry.yaml # PolicyAutomation — alerts on certificate expiry
│
├── manifests/                         # Raw K8s manifests (input to PolicyGenerator)
│   ├── 01-namespace.yaml
│   ├── 02-serviceaccount.yaml
│   ├── 03-clusterrole.yaml
│   ├── 04-clusterrolebinding.yaml
│   ├── 05-deployment.yaml             # OIDC proxy Deployment (uses ACM hub templates)
│   ├── 06-service.yaml
│   ├── 07-route.yaml                  # Route (uses ACM hub templates for hostname)
│   ├── 08-authentication-cr.yaml      # Changes serviceAccountIssuer (WARNING: restarts API)
│   ├── 09-cert-manager-certificate.yaml
│   ├── 10-certificate-policy.yaml     # CertificatePolicy (cert expiry monitoring)
│   └── 11-external-infra-check.yaml   # Canary ConfigMap check (triggers Ansible)
│
├── argocd/
│   └── application.yaml               # ArgoCD Application for GitOps deployment
│
├── secrets/
│   └── aap-credentials-secret.yaml.template  # AAP credential template (DO NOT COMMIT actual secret)
│
└── ansible/                           # Ansible playbooks for AAP (separate Git project in AAP)
    ├── requirements.yml               # Ansible collection dependencies
    ├── playbooks/
    │   ├── oidc-full-onboarding.yml   # Main workflow playbook
    │   └── per-cluster-setup.yml      # Per-cluster tasks (F5, DNS, Entra, ConfigMap)
    ├── roles/
    │   ├── f5_virtual_server/         # F5 BIG-IP VIP, pool, health monitor, TLS
    │   ├── dns_record/                # DNS A record (MUST customize for your provider)
    │   ├── azure_entra_federation/    # Azure Entra federated identity credential
    │   └── k8s_infra_status/          # Creates feedback ConfigMap on managed cluster
    └── group_vars/
        └── all.yml                    # Variables (MUST customize before use)
```

## Prerequisites

1. **ACM Hub Cluster** with:
   - Red Hat Advanced Cluster Management installed
   - Ansible Automation Platform Resource Operator installed (from OperatorHub)
   - A `ManagedClusterSet` containing your target clusters

2. **Ansible Automation Platform Controller** with:
   - A Job Template named `OIDC-External-Infra-Onboarding` pointing to `ansible/playbooks/oidc-full-onboarding.yml`
   - A Job Template named `OIDC-Certificate-Expiry-Alert` for certificate alerting
   - Required Ansible collections installed (see `ansible/requirements.yml`)

3. **cert-manager** installed on managed clusters (deploy via a separate ACM policy)

4. **AAP Credential Secret** applied to the hub (not stored in git):
   ```bash
   cp secrets/aap-credentials-secret.yaml.template secrets/aap-credentials-secret.yaml
   # Edit with your actual AAP token and host URL
   oc apply -f secrets/aap-credentials-secret.yaml
   ```

## Deployment

### Option A: ArgoCD / OpenShift GitOps (recommended)

Use the centralized App of Apps pattern from the repository root, which deploys
all policy domains (shared, cert-manager, external, internal) with correct
ordering via sync waves:

```bash
oc apply -f argocd/app-of-apps.yaml
```

Alternatively, deploy only the external policies independently:

```bash
oc apply -f argocd/applications/02-external-policies.yaml
```

See [DEPLOYMENT_GUIDE.md](../DEPLOYMENT_GUIDE.md) for the full GitOps walkthrough.

### Option B: Manual Kustomize Build

```bash
kustomize build --enable-alpha-plugins . | oc apply -f - -n acm-policies
```

### Option C: ACM Subscription (Application Lifecycle)

ACM's built-in Subscription model natively supports the PolicyGenerator
Kustomize plugin. Create a Channel + Subscription pointing to this repository.

## Onboarding a New Cluster

After deployment, onboarding a cluster is a single label:

```bash
oc label managedcluster <cluster-name> oidc-federation=enabled --overwrite
```

This triggers the full automated pipeline:

1. ACM Placement includes the cluster
2. Policies 1-7 deploy in-cluster resources (namespace, RBAC, proxy, route, issuer, cert)
3. Policy 8 goes NonCompliant (ConfigMap missing)
4. PolicyAutomation triggers Ansible in AAP
5. Ansible configures F5 + DNS + Entra, creates ConfigMap
6. Policy 8 becomes Compliant, automation stops
7. All 8 policies green in Governance dashboard

## What You MUST Customize

The following placeholders (`<REPLACE:>`) must be set before deployment:

| File | What to change |
|------|---------------|
| `manifests/05-deployment.yaml` | Container image URL |
| `manifests/09-cert-manager-certificate.yaml` | ClusterIssuer name |
| `policy-automation.yaml` | `azure_tenant_id`, `azure_app_id` |
| `argocd/application.yaml` | Git repository URL |
| `secrets/aap-credentials-secret.yaml.template` | AAP token and host |
| `ansible/group_vars/all.yml` | All F5, Azure, DNS, and cluster access variables |
| `ansible/roles/dns_record/tasks/main.yml` | Uncomment your DNS provider |

## ACM Hub Templates

Several manifests use ACM hub-side template syntax (`{{ ... }}`). These are
evaluated by ACM when the policy is applied to each managed cluster. The
template dynamically derives the cluster-specific OIDC issuer URL from
the cluster's `Infrastructure` CR. No per-cluster YAML files are needed.

## Important Warnings

- **`08-authentication-cr.yaml`**: Changing `serviceAccountIssuer` causes API
  server pods to restart and invalidates existing SA tokens. Plan accordingly.
- **`ansible/roles/dns_record/`**: Ships as a placeholder. You must implement
  the actual DNS record creation logic for your DNS provider.
- **Secrets**: Never commit `secrets/*.yaml` to git. The `.gitignore` excludes them.

## API Versions Used

| Resource | API Version | Notes |
|----------|------------|-------|
| Placement | `cluster.open-cluster-management.io/v1beta1` | Current API (not deprecated PlacementRule) |
| ManagedClusterSetBinding | `cluster.open-cluster-management.io/v1beta2` | Required for Placement |
| PolicyGenerator | `policy.open-cluster-management.io/v1` | Kustomize plugin |
| PolicyAutomation | `policy.open-cluster-management.io/v1beta1` | Triggers Ansible |
| CertificatePolicy | `policy.open-cluster-management.io/v1` | ACM cert monitoring |
| ConfigurationPolicy | `policy.open-cluster-management.io/v1` | ACM config enforcement |
