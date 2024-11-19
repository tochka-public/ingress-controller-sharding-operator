package v1

import (
	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type HTTPProxyTemplateSpec struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the behavior of a HTTPProxy.
	// +optional
	Spec contourv1.HTTPProxySpec `json:"spec,omitempty"`
}

// ShardedHTTPProxySpec defines the desired state of ShardedHTTPProxy
type ShardedHTTPProxySpec struct {
	Template HTTPProxyTemplateSpec `json:"template,omitempty"`
}

// ShardedHTTPProxyStatus defines the observed state of ShardedHTTPProxy
type ShardedStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// +kubebuilder:default:={}
	CreatedObjects map[string][]map[string]string `json:"createdObjects"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Class",type="string",JSONPath=".spec.template.spec.ingressClassName",description="Class of the Ingress resource"

// ShardedHTTPProxy is the Schema for the shardedhttpproxies API
type ShardedHTTPProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ShardedHTTPProxySpec `json:"spec,omitempty"`
	Status ShardedStatus        `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ShardedHTTPProxyList contains a list of ShardedHTTPProxy
type ShardedHTTPProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ShardedHTTPProxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ShardedHTTPProxy{}, &ShardedHTTPProxyList{})
}

func (s *ShardedHTTPProxy) GetCreatedObjects() *map[string][]map[string]string {
	return &s.Status.CreatedObjects
}

func (s *ShardedHTTPProxy) SetCreatedObjects(new map[string][]map[string]string) {
	s.Status.CreatedObjects = new
}

func (s *ShardedHTTPProxy) GetObject() client.Object {
	return s
}

func (s *ShardedHTTPProxy) GetIngressClassName() string {
	return s.Spec.Template.Spec.IngressClassName
}

func (s *ShardedHTTPProxy) GetChildKind() string {
	return contourv1.HTTPProxy{}.Kind
}

func (s *ShardedHTTPProxy) GetKind() string {
	return s.Kind
}
