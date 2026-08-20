package config

import "k8s.io/apimachinery/pkg/runtime/schema"

// GVR converts a KubernetesSourceSpec's group/version/resource fields into
// a schema.GroupVersionResource for use with a dynamic client.
func (k *KubernetesSourceSpec) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: k.Group, Version: k.Version, Resource: k.Resource}
}
