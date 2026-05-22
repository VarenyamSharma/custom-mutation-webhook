# custom-mutation-webhook

> A Kubernetes **Mutating Admission Webhook** written in Go that automatically injects labels into pods at creation time — designed for local development with [`kind`](https://kind.sigs.k8s.io/).

---

## Overview

When a Pod creation request hits the Kubernetes API server, this webhook intercepts it, injects the label `example-webhook: it-worked`, and lets the request proceed — all transparently, before the pod is scheduled.

This project demonstrates how to build, deploy, and test a production-style admission webhook without needing a remote registry. Everything runs inside a local `kind` cluster.

```
Kubernetes API ──[Pod CREATE]──► MutatingWebhookConfiguration ──► Webhook Service (Go / TLS)
                                                                         │
         Active Pod ◄───────────[Apply Patch]◄────[AdmissionResponse] ◄─┘
       (Label Injected)               (JSON Patch, base64-encoded)
```

---

## How It Works

1. **Intercept** — A Pod manifest is applied. The API server checks registered `MutatingWebhookConfigurations` and forwards the request to the webhook service at `/mutate`.
2. **Forward** — The API server sends a `POST` containing an `AdmissionReview` JSON payload.
3. **Process** — The Go server:
   - Decodes the `AdmissionReview` into a typed struct.
   - Unmarshals the raw Pod spec.
   - Builds a JSON Patch (`op: add`, `path: /metadata/labels`).
4. **Respond** — Returns an `AdmissionResponse` with the base64-encoded patch and `allowed: true`.
5. **Mutate** — Kubernetes applies the patch; the Pod is created with the injected label.

---

## Project Structure

```
kubernetes/admissioncontrollers/introduction/
├── sourcecode/
│   ├── main.go          # HTTPS server, TLS setup, in-cluster / kubeconfig mode, handler registration
│   ├── test.go          # Startup connectivity check — lists pods to verify K8s client
│   └── Dockerfile       # Multi-stage build (Go 1.21 → Alpine)
├── tls/                 # Local CA config and TLS key/cert files
├── deployment.yaml      # Deployment + Service for the webhook server
├── webhook.yaml         # MutatingWebhookConfiguration — registers the webhook with K8s
├── rbac.yaml            # ServiceAccount, ClusterRole, ClusterRoleBinding
├── demo-pod.yaml        # Test pod with trigger label `example-webhook-enabled: "true"`
└── mock-request.json    # Captured AdmissionReview payload for local curl testing
```

---

## Prerequisites

| Tool | Purpose |
|------|---------|
| [`kind`](https://kind.sigs.k8s.io/) | Local Kubernetes cluster |
| [`kubectl`](https://kubernetes.io/docs/tasks/tools/) | Cluster interaction |
| [Docker](https://docs.docker.com/get-docker/) | Image build |
| [Go 1.21+](https://go.dev/dl/) | Local development / testing |

---

## Getting Started

All commands assume you are inside `kubernetes/admissioncontrollers/introduction/`.

### 1. Create a `kind` Cluster

```bash
kind create cluster --name webhook
```

### 2. Build the Docker Image

```bash
docker build -t example-webhook:v1 ./sourcecode
```

### 3. Load the Image into Kind

No registry needed — load the image directly into the cluster node:

```bash
kind load docker-image example-webhook:v1 --name webhook
```

### 4. Create the TLS Secret

Kubernetes requires admission webhooks to be served over HTTPS:

```bash
kubectl -n default apply -f ./tls/example-webhook-tls.yaml
```

### 5. Apply RBAC Rules

Grant the webhook pod permissions to read pods:

```bash
kubectl -n default apply -f rbac.yaml
```

### 6. Deploy the Webhook Server

```bash
kubectl -n default apply -f deployment.yaml
kubectl get pods   # Verify the webhook pod is Running
```

### 7. Register the Webhook

Once the server is healthy, register it with the API server:

```bash
kubectl -n default apply -f webhook.yaml
```

---

## Testing

Apply a test pod that carries the trigger label `example-webhook-enabled: "true"`:

```bash
kubectl -n default apply -f demo-pod.yaml
```

Verify label injection:

```bash
kubectl get pods --show-labels
```

Expected output includes `example-webhook=it-worked` in the labels column for `demo-pod`.

---

## Local Development & Debugging

Rebuilding a Docker image on every code change is slow. The webhook saves the raw `AdmissionReview` payload it receives to `/tmp/request` inside the container. Use this to iterate locally in seconds:

**Step 1 — Capture a real request from the running pod:**

```bash
kubectl cp <webhook-pod-name>:/tmp/request ./mock-request.json
```

**Step 2 — Run the server locally against your cluster:**

```bash
USE_KUBECONFIG=1 go run ./sourcecode/main.go ./sourcecode/test.go
```

**Step 3 — Replay the captured payload:**

```bash
curl -k -X POST -d @mock-request.json https://localhost:8443/mutate
```

This loop eliminates Docker builds and Kubernetes redeployments during active development.

---

## License

This project is intended for educational and demonstration purposes.
