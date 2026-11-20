/*
Copyright 2024 The Ketches Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path"

	"github.com/ketches/registry-proxy/internal/config"
	"github.com/ketches/registry-proxy/internal/global"
	"github.com/ketches/registry-proxy/internal/rules"
	"github.com/ketches/registry-proxy/pkg/image"
	"github.com/ketches/registry-proxy/pkg/util"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Run starts the registry-proxy admission webhook.
func Run() {
	// Initialization (ConfigMap informer, optional file watcher, TLS and
	// webhook registration) happens explicitly here rather than in a package
	// init function, so importing the package has no cluster or filesystem
	// side effects and unit tests can exercise the handlers in isolation.
	Init()

	http.HandleFunc(global.WebhookServicePath, mutatePod)
	log.Println("Start serving registry-proxy admission webhook ...")

	if err := http.ListenAndServeTLS(":443", global.WebhookServiceTLSCertFile, global.WebhookServiceTLSKeyFile, nil); err != nil {
		log.Fatalln("Failed to listen and serve admission webhook.", err.Error())
	}
}

// mutatePod is the handler of the admission webhook.
func mutatePod(w http.ResponseWriter, r *http.Request) {
	log.Println("Request admission webhook mutating ...")

	request, pod, err := parseRequest(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not parse request: %v", err), http.StatusBadRequest)
		return
	}

	// If the pod not match the pod selector, return directly.
	if selector := config.PodSelector().AsSelector(); !selector.Empty() {
		if !selector.Matches(labels.Set(pod.Labels)) {
			response(w, request, nil)
			return
		}
	}

	// Take the rule snapshot exactly once for the whole admission request.
	// Every container lookup below uses this same immutable snapshot, so a
	// concurrent hot reload can never make a pod see old rules for one
	// container and new rules for another.
	snapshot := config.Engine().Snapshot()

	patchBytes, err := patchPod(pod, snapshot)
	if err != nil {
		log.Println("Marshal patch failed.")
		http.Error(w, fmt.Sprintf("could not marshal patch: %v", err), http.StatusInternalServerError)
		return
	}

	response(w, request, patchBytes)
}

// parseRequest parses the request of the admission webhook.
func parseRequest(r *http.Request) (*admissionv1.AdmissionReview, *corev1.Pod, error) {
	var (
		request admissionv1.AdmissionReview
		pod     = &corev1.Pod{}
	)

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		log.Println("Decode body failed.")
		return nil, nil, fmt.Errorf("could not decode body: %v", err)
	}

	raw := request.Request.Object.Raw

	if err := json.Unmarshal(raw, pod); err != nil {
		log.Println("Unmarshal pod object failed.", err.Error())
		return nil, nil, fmt.Errorf("could not unmarshal pod object: %v", err)
	}

	return &request, pod, nil
}

// patchPod generates the patch for the pod.
func patchPod(pod *corev1.Pod, snapshot *rules.Snapshot) ([]byte, error) {
	// If the pod is controlled by a controller, set podName as generateName.
	podName := util.ValueIf(pod.Name != "", pod.Name, pod.GenerateName)

	log.Printf("Pod %s/%s is included", pod.Namespace, podName)

	replaceImage(pod, snapshot)

	var patches = []map[string]any{
		{
			"op":    "replace",
			"path":  "/spec/initContainers",
			"value": pod.Spec.InitContainers,
		},
		{
			"op":    "replace",
			"path":  "/spec/containers",
			"value": pod.Spec.Containers,
		},
	}

	return json.Marshal(patches)
}

// response sends the response to the admission webhook.
func response(w http.ResponseWriter, request *admissionv1.AdmissionReview, patchBytes []byte) {
	response := &admissionv1.AdmissionReview{
		TypeMeta: request.TypeMeta,
		Response: &admissionv1.AdmissionResponse{
			UID:     request.Request.UID,
			Allowed: true,
			Result:  nil,
		},
	}

	if len(patchBytes) > 0 {
		response.Response.Patch = patchBytes
		response.Response.PatchType = func() *admissionv1.PatchType {
			pt := admissionv1.PatchTypeJSONPatch
			return &pt
		}()
	}

	encodeResponse(w, response)
}

// encodeResponse encodes the response to the admission webhook.
func encodeResponse(w http.ResponseWriter, response *admissionv1.AdmissionReview) {
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Println("Encode response failed.")
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
}

// replaceImage replaces the images in the pod using the request-scoped rule
// snapshot.
func replaceImage(pod *corev1.Pod, snapshot *rules.Snapshot) {
	for i := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[i].Image = getProxyImage(pod.Spec.InitContainers[i].Image, snapshot)
	}

	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].Image = getProxyImage(pod.Spec.Containers[i].Image, snapshot)
	}

	for i := range pod.Spec.EphemeralContainers {
		pod.Spec.EphemeralContainers[i].EphemeralContainerCommon.Image = getProxyImage(pod.Spec.EphemeralContainers[i].EphemeralContainerCommon.Image, snapshot)
	}
}

// getProxyImage rewrites the raw image against the immutable snapshot. The
// image is first parsed into registry + name so an implicit docker.io host
// (e.g. "nginx:latest") is normalized before rule matching; the engine then
// applies at most one longest-prefix rewrite and preserves the tag/digest.
func getProxyImage(rawImage string, snapshot *rules.Snapshot) string {
	result := rawImage
	registry, name, err := image.Parse(rawImage)
	if err != nil {
		log.Println("Parse image failed.")
		return result
	}

	ref := path.Join(registry, name)
	rewritten, ok := snapshot.Rewrite(ref)
	if !ok {
		return result
	}
	if rewritten != rawImage {
		log.Printf("Proxy image: %s -> %s", rawImage, rewritten)
	}
	return rewritten
}
