# spire-registrar

A Kubernetes controller that automates SPIFFE/SPIRE registration entry lifecycle management for workloads. When a `ServiceAccount` is annotated with `omegahome.net/managed-spire: "true"`, the controller automatically registers a SPIFFE ID with the SPIRE server and stores the entry ID back on the ServiceAccount. It also ensures cleanup of the SPIRE registration entry when the ServiceAccount is deleted.

## Table of Contents

- [Architecture](#architecture)
- [How It Works](#how-it-works)
- [Prerequisites](#prerequisites)
- [Cluster Pre-configuration](#cluster-pre-configuration)
- [Installation](#installation)
- [Usage](#usage)
- [Configuration Reference](#configuration-reference)
- [RBAC](#rbac)
- [Development](#development)

---

## Architecture

```
+------------------------------------------------------------------+
|                       Kubernetes Cluster                         |
|                                                                  |
|  +--------------------------------------------------------------+|
|  |  spire-registrar (controller-manager Deployment)            ||
|  |                                                              ||
|  |   +---------------------------------------------------------+||
|  |   |  ServiceAccountReconciler                               |||
|  |   |  Watch: ServiceAccount (all namespaces)                 |||
|  |   |  Filter: annotation omegahome.net/managed-spire         |||
|  |   +----------------------------+----------------------------+|||
|  |                                |                             ||
|  |              (reads)           |  (reads)                   ||
|  |                  v             v                             ||
|  |  +-------------------+   +----------------------+           ||
|  |  | kube-system/      |   | kube-system/         |           ||
|  |  | kubeadm-config    |   | admin-kubeconfig     |           ||
|  |  | ConfigMap         |   | Secret               |           ||
|  |  | (clusterName,     |   | (kubeconfig for      |           ||
|  |  |  trustDomain)     |   |  SPIRE attestation)  |           ||
|  |  +-------------------+   +----------------------+           ||
|  +----------------------------+---------------------------------+|
|                               |                                  |
|                               | HTTP POST                        |
|                               | /v1/entries/add                  |
|                               | /v1/entries/delete               |
+-------------------------------|----------------------------------+
                                v
              +--------------------------------------+
              |  SPIRE REST API Server               |
              |  omegaspire01.omegaworld.net:8080    |
              +--------------------------------------+
```

### Components

| Component | Description |
|---|---|
| `ServiceAccountReconciler` | Core controller that watches all `ServiceAccount` resources cluster-wide |
| `spire-api.go` | HTTP client layer that communicates with the SPIRE registration REST API |
| `kubeadm-config` ConfigMap | Source of cluster name and SPIRE trust domain (`kube-system` namespace) |
| `admin-kubeconfig` Secret | Admin kubeconfig passed to SPIRE for workload attestation (`kube-system` namespace) |

---

## How It Works

### Registration Flow (ServiceAccount creation)

```
1. User creates ServiceAccount with annotation:
      omegahome.net/managed-spire: "true"

2. Controller detects change via Watch

3. Controller reads kube-system/kubeadm-config ConfigMap:
   - Reads ClusterConfiguration YAML -> extracts clusterName
   - Reads annotation omega.k8s.io/spire-trustdomain -> extracts trustDomain

4. Controller reads kube-system/admin-kubeconfig Secret:
   - Base64-encodes the kubeconfig data

5. Controller POSTs to SPIRE API:
   POST http://omegaspire01.omegaworld.net:8080/v1/entries/add
   {
     "trustDomain": "<from kubeadm-config>",
     "serviceAccount": "<sa name>",
     "namespace": "<sa namespace>",
     "cluster": "<from kubeadm-config>",
     "kubeConfig": "<base64 kubeconfig>"
   }

6. SPIRE API returns entryID

7. Controller updates ServiceAccount annotations:
      omegahome.net/svid-entry-id: <entryID>

8. Controller adds finalizer:
      omegahome.net/spire-finalizer
```

### Cleanup Flow (ServiceAccount deletion)

```
1. User deletes ServiceAccount

2. Kubernetes sets DeletionTimestamp (finalizer blocks actual deletion)

3. Controller detects DeletionTimestamp, POSTs to SPIRE API:
   POST http://omegaspire01.omegaworld.net:8080/v1/entries/delete
   { "trustDomain": "...", "serviceAccount": "...", "namespace": "...", "cluster": "..." }

4. On success, controller removes finalizer omegahome.net/spire-finalizer

5. Kubernetes proceeds with ServiceAccount deletion
```

### Annotations Reference

| Annotation | Placed By | Description |
|---|---|---|
| `omegahome.net/managed-spire: "true"` | User | Opts the ServiceAccount into SPIRE management |
| `omegahome.net/svid-entry-id: <id>` | Controller | Stores the SPIRE entry ID returned by the API |
| `omega.k8s.io/spire-trustdomain: <domain>` | Admin (on `kubeadm-config` ConfigMap) | Defines the SPIRE trust domain for the cluster |

### Finalizers

The controller adds `omegahome.net/spire-finalizer` to every managed ServiceAccount. This guarantees the SPIRE registration entry is deleted before the ServiceAccount is removed from Kubernetes, preventing orphaned entries in the SPIRE server.

---

## Prerequisites

- Kubernetes v1.21+
- A running SPIRE server with the REST registration API exposed (default: `omegaspire01.omegaworld.net:8080`)
- Helm v3.8+ (for Helm-based installation)

---

## Cluster Pre-configuration

Two cluster-level resources must exist before installing the controller.

### 1. kubeadm-config ConfigMap (trust domain annotation)

The controller reads cluster identity from the `kubeadm-config` ConfigMap in `kube-system`. Add the trust domain annotation:

```sh
kubectl annotate configmap kubeadm-config \
  -n kube-system \
  omega.k8s.io/spire-trustdomain=<your-trust-domain>
```

Example:

```sh
kubectl annotate configmap kubeadm-config \
  -n kube-system \
  omega.k8s.io/spire-trustdomain=omegaworld.net
```

The `ClusterConfiguration` key in the ConfigMap must contain a YAML document with a `clusterName` field (standard `kubeadm init` output).

### 2. admin-kubeconfig Secret

```sh
kubectl create secret generic admin-kubeconfig \
  -n kube-system \
  --from-file=kubeconfig=/path/to/admin.kubeconfig
```

---

## Installation

### Using Helm (recommended)

```sh
helm install spire-registrar \
  oci://docker.io/shanmugara/spire-registrar \
  --namespace spire \
  --create-namespace \
  --version 1.0.9
```

#### Install from local chart

```sh
helm install spire-registrar ./charts/spire-registrar \
  --namespace spire \
  --create-namespace
```

#### Override image and enable leader election for HA

```sh
helm install spire-registrar ./charts/spire-registrar \
  --namespace spire \
  --create-namespace \
  --set image.repository=docker.io/shanmugara/spire-registrar \
  --set image.tag=1.0.9 \
  --set leaderElection.enabled=true \
  --set replicaCount=2
```

#### Upgrade

```sh
helm upgrade spire-registrar ./charts/spire-registrar \
  --namespace spire \
  --reuse-values
```

#### Uninstall

```sh
helm uninstall spire-registrar --namespace spire
```

### Using Kustomize / kubectl

```sh
make deploy IMG=docker.io/shanmugara/spire-registrar:1.0.9
```

To remove:

```sh
make undeploy
```

---

## Usage

Annotate any `ServiceAccount` to have the controller register a SPIFFE ID for it:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: my-workload
  namespace: my-app
  annotations:
    omegahome.net/managed-spire: "true"
```

Apply it:

```sh
kubectl apply -f my-serviceaccount.yaml
```

Once reconciled, the controller adds the SPIRE entry ID back to the ServiceAccount:

```sh
kubectl get sa my-workload -n my-app -o jsonpath='{.metadata.annotations}'
# Output:
# {"omegahome.net/managed-spire":"true","omegahome.net/svid-entry-id":"<entry-id>"}
```

To remove the SPIRE registration, delete the ServiceAccount:

```sh
kubectl delete sa my-workload -n my-app
```

The controller calls the SPIRE API to delete the entry before allowing Kubernetes to complete the deletion.

---

## Configuration Reference

### Helm values

| Value | Default | Description |
|---|---|---|
| `replicaCount` | `1` | Number of controller replicas |
| `image.repository` | `docker.io/shanmugara/spire-registrar` | Controller image repository |
| `image.tag` | `""` (uses Chart appVersion) | Image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Image pull secrets |
| `serviceAccount.create` | `true` | Create a ServiceAccount for the controller pod |
| `serviceAccount.name` | `""` | Override ServiceAccount name |
| `serviceAccount.annotations` | `{}` | Annotations for the controller ServiceAccount |
| `leaderElection.enabled` | `false` | Enable leader election (set `true` for `replicaCount > 1`) |
| `metrics.port` | `8080` | Port for Prometheus metrics endpoint |
| `metrics.secure` | `false` | Serve metrics over HTTPS |
| `healthProbe.port` | `8081` | Port for liveness and readiness probes |
| `resources.limits.cpu` | `500m` | CPU limit |
| `resources.limits.memory` | `128Mi` | Memory limit |
| `resources.requests.cpu` | `10m` | CPU request |
| `resources.requests.memory` | `64Mi` | Memory request |
| `podAnnotations` | `{}` | Additional pod annotations |
| `nodeSelector` | `{}` | Node selector |
| `tolerations` | `[]` | Pod tolerations |
| `affinity` | `{}` | Pod affinity rules |

### Controller flags

| Flag | Default | Description |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Address for the metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Address for health probes |
| `--leader-elect` | `false` | Enable leader election |
| `--metrics-secure` | `false` | Serve metrics securely |
| `--enable-http2` | `false` | Enable HTTP/2 (disabled by default - mitigates stream cancellation CVEs) |

---

## RBAC

The controller requires the following cluster-level permissions:

| Resource | Verbs | Purpose |
|---|---|---|
| `serviceaccounts` | `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` | Watch and update managed ServiceAccounts |
| `serviceaccounts/finalizers` | `update` | Add/remove the SPIRE cleanup finalizer |
| `serviceaccounts/status` | `get`, `patch`, `update` | Update ServiceAccount status |
| `configmaps` | `get` | Read `kube-system/kubeadm-config` for cluster name and trust domain |
| `secrets` | `get` | Read `kube-system/admin-kubeconfig` for SPIRE attestation |

---

## Development

### Prerequisites

- Go v1.21+
- Docker v24+
- `kubectl` v1.21+

### Build and run locally

```sh
# Run unit tests
make test

# Build binary
make build

# Run the controller locally (uses current kubeconfig context)
make run
```

### Build and push Docker image

```sh
make docker-build docker-push IMG=<your-registry>/spire-registrar:tag
```

### Deploy with Kustomize

```sh
make deploy IMG=<your-registry>/spire-registrar:tag
```

### Project structure

```
cmd/
  main.go                               # Entry point, manager bootstrap
internal/
  controller/
    serviceaccount_controller.go        # Reconciler: watch, lifecycle, finalizers
    spire-api.go                        # SPIRE REST API client, cluster info helpers
    suite_test.go                       # Test suite bootstrap
    serviceaccount_controller_test.go   # Controller unit tests
charts/
  spire-registrar/                      # Helm chart
    Chart.yaml
    values.yaml
    templates/
      deployment.yaml
      serviceaccount.yaml
      clusterrole.yaml
      clusterrolebinding.yaml
      service.yaml
      _helpers.tpl
config/
  manager/                              # Kustomize deployment manifests
  rbac/                                 # RBAC manifests
  default/                              # Kustomize default overlay
```
