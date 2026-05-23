# Mutating Admission Webhook

This is a mutating admission webhook for Kubernetes. It intercepts pod creation requests and automatically injects a label (`example-webhook: it-worked`) onto the pod before it gets scheduled.

We built this to run locally inside a `kind` cluster, handling the TLS requirements and local image loading without needing to push to a public registry like Docker Hub.

---

## How It Works

```
Kubernetes API ──[Pod CREATE]──► MutatingWebhook ──► Webhook Pod (Go Server)
                                                         │
   Active Pod ◄───[Apply Patch]◄─── [Allowed: true] ◄────┘
 (Injected Label)                 (JSON Patch Bytes)
```

1. **Intercept**: When you apply a Pod manifest (like `demo-pod.yaml`), the Kubernetes API server checks its registered `MutatingWebhookConfigurations`.
2. **Forward**: The API server sends a POST request containing an `AdmissionReview` JSON payload to our webhook service at `/mutate`.
3. **Process**: Our Go server (running on port 8443) receives the payload:
   - It decodes the `AdmissionReview` bytes into a Go struct.
   - It extracts the Pod's raw spec and unmarshals it.
   - It checks the labels and appends a JSON patch (`op: "add", path: "/metadata/labels", value: labels`).
4. **Respond**: The server sends back an `AdmissionResponse` containing the base64-encoded JSON patch.
5. **Mutate**: Kubernetes applies the patch, and the pod is created with the new label.

---

## Code Layout

- `sourcecode/main.go`: The core webhook server. Parses CLI flags, sets up TLS, decides whether to use `InClusterConfig` or local `kubeconfig`, registers handlers, and starts the HTTPS server.
- `sourcecode/test.go`: A simple helper that lists the pods in your cluster on startup to verify K8s client connectivity.
- `sourcecode/Dockerfile`: Multi-stage build that compiles the Go binary using Go 1.21 and packages it into a minimal Alpine image.
- `tls/`: Holds the local certificate authority (CA) config and the TLS keys.
- `webhook.yaml`: Registers the webhook configuration with Kubernetes.
- `deployment.yaml`: Runs our Go webhook server as a pod and exposes it via a service.

---

## Getting It Running Locally

We are using a `kind` cluster named `webhook` for local development.

### 1. Build the Docker Image
Inside the `sourcecode/` directory, compile the binary and build the docker image:
```bash
docker build -t example-webhook:v1 .
```

### 2. Load the Image into Kind
Since we are working locally, we don't want to push this to Docker Hub. Load it directly into the local `kind` cluster's node:
```bash
kind load docker-image example-webhook:v1 --name webhook
```

### 3. Create the TLS Secret
Kubernetes strictly requires admission webhooks to run over HTTPS. We store the TLS certificate and private key in a Kubernetes secret:
```bash
kubectl -n default apply -f ./tls/example-webhook-tls.yaml
```

### 4. Deploy the Webhook and RBAC Rules
Apply the ServiceAccount, ClusterRole, and ClusterRoleBinding so the webhook pod has permissions to read pods:
```bash
kubectl -n default apply -f rbac.yaml
```

Deploy the webhook server itself:
```bash
kubectl -n default apply -f deployment.yaml
```

Check that the pod is running:
```bash
kubectl get pods
```

### 5. Register the Webhook
Once the server is running, tell Kubernetes to start routing Pod creation events to it:
```bash
kubectl -n default apply -f webhook.yaml
```

---

## Testing the Webhook

Apply a test pod that has the trigger label `example-webhook-enabled: "true"`:
```bash
kubectl -n default apply -f demo-pod.yaml
```

Verify that the label was successfully injected by the webhook:
```bash
kubectl get pods --show-labels
```
You should see `example-webhook=it-worked` in the labels column for `demo-pod`.

---

## Local Development & Debugging

Deploying to Kubernetes on every single code change is slow. To speed this up, the webhook is configured to save the raw JSON requests it receives from Kubernetes.

When you create a pod, the server saves the payload to `/tmp/request` inside the container. You can copy this out to your host machine:
```bash
kubectl cp <webhook-pod-name>:/tmp/request ./mock-request.json
```

You can now run your Go server locally on your laptop:
```bash
USE_KUBECONFIG=1 go run main.go test.go
```
And test your mutation logic by sending the saved payload directly using `curl`:
```bash
curl -k -X POST -d @mock-request.json https://localhost:8443/mutate
```
This lets you iterate on your Go code in seconds without dealing with Docker builds or Kubernetes redeployments.
