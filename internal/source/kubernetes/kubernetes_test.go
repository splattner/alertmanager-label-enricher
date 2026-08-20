package kubernetes

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/splattner/alertmanager-label-enricher/internal/source"
)

var namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

func namespaceObj(name string, labels map[string]any) *unstructured.Unstructured {
	labelMap := map[string]any{}
	for k, v := range labels {
		labelMap[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":   name,
			"labels": labelMap,
		},
	}}
}

func TestLookupClusterScopedResource(t *testing.T) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{namespaceGVR: "NamespaceList"},
		namespaceObj("payments", map[string]any{"team": "platform"}),
	)

	src, err := New("ns", client, Config{GVR: namespaceGVR, Name: `{{ .Labels.namespace }}`})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := src.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !src.HasSynced() {
		t.Fatal("expected HasSynced() to be true after Start")
	}

	v, err := src.Lookup(ctx, source.LookupInput{Labels: map[string]string{"namespace": "payments"}})
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("Lookup result type = %T", v)
	}
	metadata := obj["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	if labels["team"] != "platform" {
		t.Fatalf("team label = %v", labels["team"])
	}
}

func TestLookupNotFoundReturnsNilNil(t *testing.T) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{namespaceGVR: "NamespaceList"},
	)

	src, err := New("ns", client, Config{GVR: namespaceGVR, Name: `{{ .Labels.namespace }}`})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := src.Start(ctx); err != nil {
		t.Fatal(err)
	}

	v, err := src.Lookup(ctx, source.LookupInput{Labels: map[string]string{"namespace": "does-not-exist"}})
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("Lookup result = %v, want nil for not-found", v)
	}
}

func TestLookupEmptyRenderedNameReturnsNilNil(t *testing.T) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{namespaceGVR: "NamespaceList"},
	)

	src, err := New("ns", client, Config{GVR: namespaceGVR, Name: `{{ .Labels.missing }}`})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := src.Start(ctx); err != nil {
		t.Fatal(err)
	}

	v, err := src.Lookup(ctx, source.LookupInput{})
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("Lookup result = %v, want nil when the templated name is empty", v)
	}
}
