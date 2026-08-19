package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controllerv1 "k8s.tochka.com/sharded-ingress-controller/api/v1"
)

const (
	testClassLabel           = "service-discovery/class"
	testRootLabel            = "httpproxy/root"
	testVHAnnotation         = "httpproxy/virtual-hosts"
	testUnregisterAnnotation = "service-discovery/unregister"
	testOldShardClass        = "old-class-0"
	testNewShardClass        = "new-class-0"
)

// newMigratingShardedHTTPProxy returns a ShardedHTTPProxy that has been
// switched to ingress class "new-class" while its status still records the
// child object under the old shard, i.e. mid class migration.
func newMigratingShardedHTTPProxy() *controllerv1.ShardedHTTPProxy {
	return &controllerv1.ShardedHTTPProxy{
		// TypeMeta drives GetKind, which getObjectChildren matches against the
		// children's owner references.
		TypeMeta:   metav1.TypeMeta{Kind: "ShardedHTTPProxy", APIVersion: controllerv1.GroupVersion.String()},
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
	unregisterAnnotation := testUnregisterAnnotation
	terminationPeriod := time.Minute

	r := &ShardedHTTPProxyReconciler{ShardedHTTPProxy: sharded}
	// TypeMeta drives GetChildKind, without which deleteUnlistedObjects bails
	// out before looking at any child.
	r.ChildObject = contourv1.HTTPProxy{
		TypeMeta: metav1.TypeMeta{Kind: "HTTPProxy", APIVersion: contourv1.GroupVersion.String()},
	}
	r.ShardedReconciler = ShardedReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(testScheme).
			WithStatusSubresource(&controllerv1.ShardedHTTPProxy{}).
			WithObjects(append([]client.Object{sharded}, existing...)...).
			Build(),
		Scheme:                               testScheme,
		ctx:                                  context.Background(),
		req:                                  &ctrl.Request{NamespacedName: types.NamespacedName{Namespace: sharded.Namespace, Name: sharded.Name}},
		objKey:                               sharded.Namespace + "/" + sharded.Name,
		ctrlName:                             "shardedhttpproxy",
		ShardedObject:                        sharded,
		ChildObject:                          &r.ChildObject,
		TerminationPeriod:                    &terminationPeriod,
		AdditionalServiceDiscoveryClassLabel: &classLabel,
		RootHTTPProxyLabel:                   &rootLabel,
		VirtualHostsHTTPProxyAnnotation:      &vhAnnotation,
		UnregisterAnnotation:                 &unregisterAnnotation,
		Shards:                               []Shards{{ShardNumber: 0, ShardName: testNewShardClass}},
	}
	r.initializeCache()
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

// Mid-migration the live main object must never be scheduled for deletion.
// Booked under the old shard it was missing from the current shard's status
// list, so every reconcile set auto-delete-after on it and the next one wiped
// the annotation while reconciling the spec — an endless churn in which the
// migration never completed.
func TestApplyObjectsMigrationDoesNotChurnAutoDeleteOnMain(t *testing.T) {
	g := NewWithT(t)

	ownerRef := func() []metav1.OwnerReference {
		yes := true
		return []metav1.OwnerReference{{
			APIVersion:         controllerv1.GroupVersion.String(),
			Kind:               "ShardedHTTPProxy",
			Name:               "app",
			Controller:         &yes,
			BlockOwnerDeletion: &yes,
		}}
	}
	// The children carry TypeMeta so that GetChildKind keeps resolving after
	// the reconciler reuses ChildObject as a Get target.
	childTypeMeta := metav1.TypeMeta{Kind: "HTTPProxy", APIVersion: contourv1.GroupVersion.String()}
	tmp := &contourv1.HTTPProxy{
		TypeMeta: childTypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:            "app-0-tmp",
			Namespace:       "default",
			Annotations:     map[string]string{"old-shard": testOldShardClass},
			OwnerReferences: ownerRef(),
		},
		Spec: contourv1.HTTPProxySpec{IngressClassName: testOldShardClass},
	}
	main := &contourv1.HTTPProxy{
		TypeMeta: childTypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:            "app-0",
			Namespace:       "default",
			Labels:          map[string]string{testClassLabel: testOldShardClass, testRootLabel: "true"},
			OwnerReferences: ownerRef(),
		},
		Spec: contourv1.HTTPProxySpec{IngressClassName: testOldShardClass},
	}
	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy(), tmp, main)

	var tmpDeleteAfter string
	for cycle := 1; cycle <= 3; cycle++ {
		objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
		g.Expect(err).NotTo(HaveOccurred())
		_, err = r.applyObjectsToCluster(objs)
		g.Expect(err).NotTo(HaveOccurred())

		gotMain := &contourv1.HTTPProxy{}
		g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-0"}, gotMain)).To(Succeed())
		g.Expect(gotMain.Annotations).NotTo(HaveKey(AutoDeleteAfterAnnotation),
			"cycle %d: live main object must not be scheduled for deletion", cycle)

		// The tmp object still has to run its deletion timeline, and its
		// deadline must be set once rather than rescheduled every cycle. This
		// also proves the deletion pass really ran.
		gotTmp := &contourv1.HTTPProxy{}
		g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-0-tmp"}, gotTmp)).To(Succeed())
		g.Expect(gotTmp.Annotations).To(HaveKey(AutoDeleteAfterAnnotation), "cycle %d", cycle)
		if tmpDeleteAfter == "" {
			tmpDeleteAfter = gotTmp.Annotations[AutoDeleteAfterAnnotation]
		} else {
			g.Expect(gotTmp.Annotations[AutoDeleteAfterAnnotation]).To(Equal(tmpDeleteAfter),
				"cycle %d: tmp auto-delete-after must not be rescheduled", cycle)
		}
	}
}
