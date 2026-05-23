// This program is a Kubernetes Mutating Admission Webhook server.
//
// How admission webhooks work:
//  1. You create a MutatingWebhookConfiguration in Kubernetes that tells the API server:
//     "Before you accept any Pod creation, send it to THIS server first."
//  2. The Kubernetes API server sends every matching resource (e.g. Pod) to our /mutate endpoint
//     as an AdmissionReview JSON payload over HTTPS.
//  3. We inspect and/or mutate the resource (e.g. inject a sidecar container, add labels).
//  4. We reply with an AdmissionReview response containing JSON Patch operations.
//  5. Kubernetes applies those patches to the resource, then proceeds with creation.
//
// This file sets up the HTTPS server, authenticates with Kubernetes,
// and implements the two HTTP handlers the webhook needs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	// v1 provides the AdmissionReview, AdmissionRequest, and AdmissionResponse
	// types — the envelope format Kubernetes uses to send webhook payloads to us.
	"k8s.io/api/admission/v1"

	// apiv1 (aliased from k8s.io/api/core/v1) gives us the Pod struct so we can
	// unmarshal and inspect the pod that Kubernetes is asking us to review.
	apiv1 "k8s.io/api/core/v1"

	// runtime and serializer together form the Kubernetes object codec system.
	// runtime.Scheme is a registry that maps API version strings (e.g. "admission.k8s.io/v1beta1")
	// to concrete Go types. serializer.CodecFactory uses that registry to build
	// encoders/decoders for any registered type.
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	// kubernetes is the typed client for all Kubernetes API groups (pods, services, etc.).
	// rest holds the low-level connection config (server URL, TLS, auth token).
	// clientcmd parses kubeconfig files — the same format kubectl uses.
	// homedir resolves "~" to the real home directory in a cross-platform way.
	"k8s.io/client-go/kubernetes"
	rest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// ServerParameters holds the values parsed from CLI flags.
// They control which port to listen on and where to find the TLS certificate
// and key files required for HTTPS (Kubernetes only calls webhooks over TLS).
type ServerParameters struct {
	port     int    // TCP port the webhook server listens on (default: 8443)
	certFile string // Path to the PEM-encoded TLS certificate file
	keyFile  string // Path to the PEM-encoded TLS private key file
}

// patchOperation represents a single JSON Patch operation (RFC 6902).
//
// JSON Patch is how we tell Kubernetes what changes to apply to the incoming resource.
// Each operation has:
//   - Op:    the action — "add", "remove", "replace", "copy", or "move"
//   - Path:  a JSON Pointer (e.g. "/spec/containers/0/image") identifying the target field
//   - Value: the new value to set (omitted for "remove" operations)
//
// Example — add a label:
//
//	patchOperation{Op: "add", Path: "/metadata/labels/env", Value: "production"}
type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// parameters holds the parsed CLI flag values for the running server instance.
var parameters ServerParameters

// scheme is the Kubernetes type registry for this server.
// By default a new scheme is empty — it knows nothing. We must explicitly register
// the API groups we want to handle. Here we register v1 admission types so
// the deserializer can map the JSON field "apiVersion: admission.k8s.io/v1"
// to the concrete Go type v1.AdmissionReview.
var scheme = runtime.NewScheme()

// universalDeserializer is initialised once at startup via an immediately-invoked
// function literal. The steps are:
//  1. v1.AddToScheme(scheme) — registers AdmissionReview and related types
//     into our scheme so the codec factory knows about them.
//  2. serializer.NewCodecFactory(scheme) — builds a factory that can produce
//     encoders/decoders for every type registered in the scheme.
//  3. .UniversalDeserializer() — returns a single Decoder that accepts any
//     registered type; it figures out the right Go struct from the JSON's
//     "apiVersion" and "kind" fields automatically.
var universalDeserializer = func() runtime.Decoder {
	_ = v1.AddToScheme(scheme)
	return serializer.NewCodecFactory(scheme).UniversalDeserializer()
}()

// config is the low-level Kubernetes REST connection config.
// It stores the API server address, CA certificate, and auth credentials
// (either a service-account token when running in-cluster, or user credentials
// from a kubeconfig file when running locally).
var config *rest.Config

// clientSet is the high-level Kubernetes API client built from config.
// It exposes typed methods for every resource group, e.g.:
//
//	clientSet.CoreV1().Pods("default").List(...)
//	clientSet.AppsV1().Deployments("default").Get(...)
//
// In this webhook we don't yet make outbound API calls, but clientSet is
// initialised here so handlers can use it later (e.g. to look up ConfigMaps).
var clientSet *kubernetes.Clientset

func main() {
	// Read environment variables that control how we authenticate with Kubernetes.
	//
	// USE_KUBECONFIG — when set to any non-empty value, switches to local dev mode:
	//                  we read credentials from a kubeconfig file instead of the
	//                  in-cluster service account token.
	//
	// KUBECONFIG     — overrides the default kubeconfig path (~/.kube/config).
	//                  Only consulted when USE_KUBECONFIG is set.
	useKubeConfig := os.Getenv("USE_KUBECONFIG")
	kubeConfigFilePath := os.Getenv("KUBECONFIG")

	// Register CLI flags so operators can customise the server at launch time
	// without rebuilding the binary. flag.Parse() below reads os.Args and fills
	// the parameters struct with whatever was passed (or the defaults).
	flag.IntVar(&parameters.port, "port", 8443, "Webhook server port.")
	flag.StringVar(&parameters.certFile, "tlsCertFile", "/etc/webhook/certs/tls.crt", "File containing the x509 Certificate for HTTPS.")
	flag.StringVar(&parameters.keyFile, "tlsKeyFile", "/etc/webhook/certs/tls.key", "File containing the x509 private key to --tlsCertFile.")
	flag.Parse()

	if len(useKubeConfig) == 0 {
		// --- PRODUCTION / IN-CLUSTER MODE ---
		// When this binary runs as a Pod, Kubernetes automatically mounts a
		// service account token at /var/run/secrets/kubernetes.io/serviceaccount/.
		// rest.InClusterConfig() reads that token plus the cluster's CA cert and
		// the API server's address from well-known environment variables
		// (KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT), producing a
		// ready-to-use *rest.Config with no manual configuration required.
		c, err := rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
		config = c
	} else {
		// --- DEVELOPMENT / LOCAL MODE ---
		// We use a kubeconfig file (same format as kubectl) to authenticate.
		// Priority: explicit KUBECONFIG env var → default ~/.kube/config.
		var kubeconfig string
		if kubeConfigFilePath == "" {
			// homedir.HomeDir() resolves the current user's home directory
			// cross-platform (works on Linux, macOS, and Windows).
			if home := homedir.HomeDir(); home != "" {
				kubeconfig = filepath.Join(home, ".kube", "config")
			}
		} else {
			kubeconfig = kubeConfigFilePath
		}

		fmt.Println("kubeconfig: " + kubeconfig)

		// clientcmd.BuildConfigFromFlags parses the kubeconfig file and returns
		// a *rest.Config pointing at whichever cluster is set as "current-context".
		// The first argument (master URL) is left empty so the kubeconfig's
		// server field is used instead.
		c, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			panic(err.Error())
		}
		config = c
	}

	// Build the typed Kubernetes client from the connection config.
	// kubernetes.NewForConfig wraps the raw REST client in typed helpers for
	// every API group (Core, Apps, Batch, RBAC, etc.).
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	clientSet = cs

	// Register HTTP route handlers:
	//   GET /        → HandleRoot   — simple liveness probe / sanity check
	//   POST /mutate → HandleMutate — the real webhook endpoint called by Kubernetes
	http.HandleFunc("/", HandleRoot)
	http.HandleFunc("/mutate", HandleMutate)

	// Start the TLS server. ListenAndServeTLS:
	//   1. Loads the certificate and key files specified by certFile/keyFile.
	//   2. Listens on the given TCP port.
	//   3. Serves each incoming connection over TLS (HTTPS).
	// Kubernetes requires webhooks to be served over HTTPS; plain HTTP is rejected.
	// log.Fatal prints the error and calls os.Exit(1) if the server ever stops.
	log.Fatal(http.ListenAndServeTLS(
		":"+strconv.Itoa(parameters.port),
		parameters.certFile,
		parameters.keyFile,
		nil, // nil means use the default http.ServeMux we registered handlers on above
	))
}

// HandleRoot handles GET / requests.
// It serves as a simple liveness probe: if you can reach this endpoint and get
// a 200 OK, the server process is up and its TLS certificate is valid.
func HandleRoot(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("HandleRoot!"))
}

// HandleMutate handles POST /mutate requests sent by the Kubernetes API server.
//
// Full request/response lifecycle:
//  1. Read the raw HTTP body (the AdmissionReview JSON sent by Kubernetes).
//  2. Persist the raw body to /tmp/request for offline debugging.
//  3. Decode the body into a typed AdmissionReview struct.
//  4. Extract and unmarshal the Pod object embedded inside the review.
//  5. Build a list of JSON Patch operations that describe our mutations.
//  6. Wrap the patches into an AdmissionReview response and write it back.
func HandleMutate(w http.ResponseWriter, r *http.Request) {

	// ── Step 1: Read the full request body ───────────────────────────────────
	// io.ReadAll drains r.Body into a byte slice. The body is the raw JSON of
	// the AdmissionReview object that Kubernetes sent us. We must read it all
	// before we can decode it; r.Body is a streaming reader, not a seekable buffer.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "could not read request body: %v", err)
		return
	}

	// ── Step 2: Persist the raw body for debugging ───────────────────────────
	// Writing the raw AdmissionReview JSON to /tmp/request lets you inspect
	// exactly what Kubernetes sent us (useful during development with kubectl).
	// os.WriteFile atomically creates or truncates the file and writes the bytes.
	// 0644 = owner can read+write, group and others can read.
	if err := os.WriteFile("/tmp/request", body, 0644); err != nil {
		panic(err.Error())
	}

	// ── Step 3: Decode the AdmissionReview envelope ──────────────────────────
	// universalDeserializer.Decode inspects the JSON's "apiVersion" and "kind"
	// fields, looks them up in our scheme, and unmarshals the bytes into the
	// matching Go struct — in this case, *v1.AdmissionReview.
	// We pass &admissionReviewReq as the target object so the result is written
	// directly into our local variable.
	var admissionReviewReq v1.AdmissionReview

	if _, _, err := universalDeserializer.Decode(body, nil, &admissionReviewReq); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "could not deserialize request: %v", err)
		return
	} else if admissionReviewReq.Request == nil {
		// The AdmissionReview wrapper decoded successfully but contains no Request.
		// This should not happen in practice but guards against malformed payloads.
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "malformed admission review: request is nil")
		return
	}

	// Log the high-level event details so operators can trace webhook activity
	// in the pod logs: what kind of resource, what operation, and its name.
	fmt.Printf("Type: %v \t Event: %v \t Name: %v \n",
		admissionReviewReq.Request.Kind,      // e.g. {Group:"", Version:"v1", Kind:"Pod"}
		admissionReviewReq.Request.Operation, // e.g. CREATE, UPDATE, DELETE
		admissionReviewReq.Request.Name,      // e.g. "my-pod"
	)

	// ── Step 4: Unmarshal the Pod from the admission request ─────────────────
	// admissionReviewReq.Request.Object.Raw holds the raw JSON bytes of the
	// Kubernetes object being admitted (a Pod in this case). We unmarshal it
	// into apiv1.Pod so we can read fields like pod.Spec.Containers, pod.Labels,
	// etc., and decide what patches (if any) to apply.
	var pod apiv1.Pod
	if err := json.Unmarshal(admissionReviewReq.Request.Object.Raw, &pod); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "could not unmarshal pod on admission request: %v", err)
		return
	}

	// ── Step 5: Build patch operations ───────────────────────────────────────
	// This is where the actual mutation logic lives. We read the pod's existing
	// labels, inject our own key ("example-webhook": "it-worked"), and describe
	// the change as a JSON Patch "add" operation on /metadata/labels.
	//
	// Why copy the whole labels map and replace it with a single "add" on
	// /metadata/labels (rather than adding a single key like
	// /metadata/labels/example-webhook)?
	// Because if the pod has NO labels at all, the /metadata/labels path doesn't
	// exist yet and a targeted add would fail. Replacing the whole map is safer.
	var patches []patchOperation

	// Copy the pod's current labels into a new map so we don't mutate the
	// in-memory struct. Then inject our sentinel label.
	labels := pod.ObjectMeta.Labels
	if labels == nil {
		// Guard: if the pod was created with no labels at all, ObjectMeta.Labels
		// is nil. We must initialise the map before writing into it.
		labels = make(map[string]string)
	}
	labels["example-webhook"] = "it-worked"

	// Append a single JSON Patch operation that replaces the entire labels map
	// with our updated copy. Op "add" on an existing key acts as a replace;
	// on a missing key/path it creates it — so "add" is correct in both cases.
	patches = append(patches, patchOperation{
		Op:    "add",
		Path:  "/metadata/labels",
		Value: labels,
	})

	// ── Step 6: Serialize patches and send the AdmissionReview response ──────

	// json.Marshal converts []patchOperation → JSON byte slice, e.g.:
	//   [{"op":"add","path":"/metadata/labels","value":{"example-webhook":"it-worked"}}]
	patchBytes, err := json.Marshal(patches)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "could not marshal JSON patch: %v", err)
		return
	}

	// PatchTypeJSONPatch tells Kubernetes to interpret our Patch bytes as
	// RFC 6902 JSON Patch (as opposed to JSON Merge Patch or Strategic Merge Patch).
	patchType := v1.PatchTypeJSONPatch

	// Build the AdmissionReview response envelope.
	// Key fields:
	//   UID       — must echo back the UID from the request so Kubernetes can
	//               correlate this response with the original request.
	//   Allowed   — true means "proceed with creating/updating the resource".
	//               Set to false (optionally with a Status reason) to reject it.
	//   PatchType — tells Kubernetes which patch format we used (JSON Patch here).
	//   Patch     — the serialised patch operations to apply before persisting.
	admissionReviewResp := v1.AdmissionReview{
		TypeMeta: admissionReviewReq.TypeMeta,
		Response: &v1.AdmissionResponse{
			UID:       admissionReviewReq.Request.UID,
			Allowed:   true,
			PatchType: &patchType,
			Patch:     patchBytes,
		},
	}

	// Serialise the whole AdmissionReview response struct back to JSON so we can
	// write it into the HTTP response body.
	resp, err := json.Marshal(admissionReviewResp)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "marshaling response: %v", err)
		return
	}

	// Set the Content-Type header so Kubernetes knows to parse the body as JSON,
	// then write the response. Kubernetes will apply our patches and admit the pod.
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}
