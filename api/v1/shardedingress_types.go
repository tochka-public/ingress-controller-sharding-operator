package v1

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// IngressTemplateSpec describes the data a ingress should have when created from a template
type IngressTemplateSpec struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the behavior of a ingress.
	// +optional
	Spec networkingv1.IngressSpec `json:"spec,omitempty"`
}

// ShardedIngressSpec defines the desired state of ShardedIngress
type ShardedIngressSpec struct {
	Template *IngressTemplateSpec `json:"template,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Class",type="string",JSONPath=".spec.template.spec.ingressClassName",description="Class of the Ingress resource"

// ShardedIngress is the Schema for the shardedingresses API
type ShardedIngress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ShardedIngressSpec `json:"spec,omitempty"`
	Status ShardedStatus      `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ShardedIngressList contains a list of ShardedIngress
type ShardedIngressList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ShardedIngress `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ShardedIngress{}, &ShardedIngressList{})
}

func (s *ShardedIngress) GetCreatedObjects() *map[string][]map[string]string {
	return &s.Status.CreatedObjects
}

func (s *ShardedIngress) SetCreatedObjects(new map[string][]map[string]string) {
	s.Status.CreatedObjects = new
}

func (s *ShardedIngress) GetObject() client.Object {
	return s
}

func (s *ShardedIngress) GetIngressClassName() string {
	return *s.Spec.Template.Spec.IngressClassName
}

func (s *ShardedIngress) GetChildKind() string {
	return networkingv1.Ingress{}.Kind
}

func (s *ShardedIngress) GetKind() string {
	return s.Kind
}
