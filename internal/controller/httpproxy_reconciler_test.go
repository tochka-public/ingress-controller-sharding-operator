package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controllerv1 "k8s.tochka.com/sharded-ingress-controller/api/v1"
)

const (
	testClassLabel    = "service-discovery/class"
	testRootLabel     = "httpproxy/root"
	testVHAnnotation  = "httpproxy/virtual-hosts"
	testOldShardClass = "old-class-0"
	testNewShardClass = "new-class-0"
)

// newMigratingShardedHTTPProxy returns a ShardedHTTPProxy that has been
// switched to ingress class "new-class" while its status still records the
// child object under the old shard, i.e. mid class migration.
func newMigratingShardedHTTPProxy() *controllerv1.ShardedHTTPProxy {
	return &controllerv1.ShardedHTTPProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: controllerv1.ShardedHTTPProxySpec{
			Template: controllerv1.HTTPProxyTemplateSpec{
				Spec: contourv1.HTTPProxySpec{
					IngressClassName: "new-class",
					VirtualHost:      &contourv1.VirtualHost{Fqdn: "app.example.com"},
				},
			},
		},
		Status: controllerv1.ShardedStatus{
			CreatedObjects: map[string][]map[string]string{
				testOldShardClass: {{"kind": "HTTPProxy", "name": "app-0"}},
			},
		},
	}
}

func newTestShardedHTTPProxyReconciler(t *testing.T, sharded *controllerv1.ShardedHTTPProxy, existing ...client.Object) *ShardedHTTPProxyReconciler {
	t.Helper()

	testScheme := runtime.NewScheme()
	if err := controllerv1.AddToScheme(testScheme); err != nil {
		t.Fatal(err)
	}
	if err := contourv1.AddToScheme(testScheme); err != nil {
		t.Fatal(err)
	}

	classLabel := testClassLabel
	rootLabel := testRootLabel
	vhAnnotation := testVHAnnotation
	terminationPeriod := time.Minute

	r := &ShardedHTTPProxyReconciler{ShardedHTTPProxy: sharded}
	r.ShardedReconciler = ShardedReconciler{
		Client:                               fake.NewClientBuilder().WithScheme(testScheme).WithObjects(existing...).Build(),
		ctx:                                  context.Background(),
		ShardedObject:                        sharded,
		ChildObject:                          &r.ChildObject,
		TerminationPeriod:                    &terminationPeriod,
		AdditionalServiceDiscoveryClassLabel: &classLabel,
		RootHTTPProxyLabel:                   &rootLabel,
		VirtualHostsHTTPProxyAnnotation:      &vhAnnotation,
		Shards:                               []Shards{{ShardNumber: 0, ShardName: testNewShardClass}},
	}
	return r
}

func findChild(t *testing.T, objs []NewChildObj, name string) (NewChildObj, *contourv1.HTTPProxy) {
	t.Helper()
	for _, o := range objs {
		if o.Obj.GetName() == name {
			return o, o.Obj.(*contourv1.HTTPProxy)
		}
	}
	t.Fatalf("child object %q not found in generated list", name)
	return NewChildObj{}, nil
}

// During class migration the tmp object must carry the OLD shard class in
// both spec.ingressClassName and the service discovery label. A regression
// here (empty class) poisons the tmp object's old-shard annotation and makes
// every subsequent reconcile wipe the class from the main child object,
// which then loops create/delete forever.
func TestNewHTTPProxiesMigrationCreatesTmpWithOldClass(t *testing.T) {
	g := NewWithT(t)

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy())
	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(2))

	_, tmp := findChild(t, objs, "app-0-tmp")
	g.Expect(tmp.Spec.IngressClassName).To(Equal(testOldShardClass))
	g.Expect(tmp.Labels).To(HaveKeyWithValue(testClassLabel, testOldShardClass))
	g.Expect(tmp.Labels).To(HaveKeyWithValue(testRootLabel, "true"))
	g.Expect(tmp.Annotations).To(HaveKeyWithValue("old-shard", testOldShardClass))

	mainChild, main := findChild(t, objs, "app-0")
	g.Expect(main.Spec.IngressClassName).To(Equal(testOldShardClass))
	g.Expect(main.Labels).To(HaveKeyWithValue(testClassLabel, testOldShardClass))
	// The main child is accounted under the new shard so that
	// deleteUnlistedObjects does not schedule it for deletion mid-migration.
	g.Expect(mainChild.ShardName).To(Equal(testNewShardClass))
}

// While the tmp object exists and its deletion window has not started, the
// main child object must keep the old shard class taken from the tmp
// object's old-shard annotation.
func TestNewHTTPProxiesMigrationKeepsOldClassWhileTmpAlive(t *testing.T) {
	g := NewWithT(t)

	tmp := &contourv1.HTTPProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "app-0-tmp",
			Namespace:   "default",
			Annotations: map[string]string{"old-shard": testOldShardClass},
		},
	}
	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy(), tmp)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))

	_, main := findChild(t, objs, "app-0")
	g.Expect(main.Spec.IngressClassName).To(Equal(testOldShardClass))
	g.Expect(main.Labels).To(HaveKeyWithValue(testClassLabel, testOldShardClass))
}

// Once the tmp object's deletion window has started, the main child object
// must switch to the new shard class.
func TestNewHTTPProxiesMigrationSwitchesToNewClassAfterWindow(t *testing.T) {
	g := NewWithT(t)

	tmp := &contourv1.HTTPProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-0-tmp",
			Namespace: "default",
			Annotations: map[string]string{
				"old-shard":               testOldShardClass,
				AutoDeleteAfterAnnotation: time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			},
		},
	}
	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy(), tmp)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))

	mainChild, main := findChild(t, objs, "app-0")
	g.Expect(main.Spec.IngressClassName).To(Equal(testNewShardClass))
	g.Expect(main.Labels).To(HaveKeyWithValue(testClassLabel, testNewShardClass))
	g.Expect(mainChild.ShardName).To(Equal(testNewShardClass))
}
