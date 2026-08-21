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
	// CreatedObjects contains currently observed child objects grouped by shard.
	// It is kept for backward compatibility with older controller versions.
	// +kubebuilder:default:={}
	CreatedObjects map[string][]map[string]string `json:"createdObjects"`

	// CurrentObjects contains currently observed child objects with detailed state.
	// +optional
	CurrentObjects []ShardedObjectStatus `json:"currentObjects,omitempty"`

	// Migration describes observed shard migration state.
	// +optional
	Migration *ShardMigrationStatus `json:"migration,omitempty"`
}

type ShardedObjectStatus struct {
	Kind              string `json:"kind,omitempty"`
	Name              string `json:"name,omitempty"`
	Namespace         string `json:"namespace,omitempty"`
	Shard             string `json:"shard,omitempty"`
	IngressClass      string `json:"ingressClass,omitempty"`
	Phase             string `json:"phase,omitempty"`
	DeleteAfter       string `json:"deleteAfter,omitempty"`
	Temporary         bool   `json:"temporary,omitempty"`
	MarkedForDeletion bool   `json:"markedForDeletion,omitempty"`
}

type ShardMigrationStatus struct {
	Active           bool                  `json:"active"`
	FromShards       []string              `json:"fromShards,omitempty"`
	ToShards         []string              `json:"toShards,omitempty"`
	StaleObjects     []ShardedObjectStatus `json:"staleObjects,omitempty"`
	TemporaryObjects []ShardedObjectStatus `json:"temporaryObjects,omitempty"`
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

func (s *ShardedHTTPProxy) SetCurrentObjects(new []ShardedObjectStatus) {
	s.Status.CurrentObjects = new
}

func (s *ShardedHTTPProxy) SetMigration(new *ShardMigrationStatus) {
	s.Status.Migration = new
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
