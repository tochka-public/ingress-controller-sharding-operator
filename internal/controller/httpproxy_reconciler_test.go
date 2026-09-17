package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	shardUpdateCooldown := 10 * time.Second

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
		ShardUpdateCooldown:                  &shardUpdateCooldown,
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

// newRegularModeShardedHTTPProxy returns a ShardedHTTPProxy whose template
// class equals the shard name, i.e. the unsharded ("Regular") layout in which
// the main child keeps the bare object name and the virtual host children are
// the ones named "app-0", "app-1", ...
func newRegularModeShardedHTTPProxy(hosts string, status map[string][]map[string]string) *controllerv1.ShardedHTTPProxy {
	sharded := &controllerv1.ShardedHTTPProxy{
		TypeMeta:   metav1.TypeMeta{Kind: "ShardedHTTPProxy", APIVersion: controllerv1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: controllerv1.ShardedHTTPProxySpec{
			Template: controllerv1.HTTPProxyTemplateSpec{
				Spec: contourv1.HTTPProxySpec{
					IngressClassName: testNewShardClass,
					VirtualHost:      &contourv1.VirtualHost{Fqdn: "app.example.com"},
				},
			},
		},
		Status: controllerv1.ShardedStatus{CreatedObjects: status},
	}
	if hosts != "" {
		sharded.Annotations = map[string]string{testVHAnnotation: hosts}
	}
	return sharded
}

// In Regular mode the main child keeps the bare object name, so the resharding
// conflict has to be looked up under "app". Keying it on "app-0" asked about
// the first virtual host child instead, which made conflict detection depend on
// that child happening to exist at index 0.
func TestCheckReshardingConflictUsesMainObjectNameInRegularMode(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("", map[string][]map[string]string{
		testOldShardClass: {{"kind": "HTTPProxy", "name": "app"}},
	})
	r := newTestShardedHTTPProxyReconciler(t, sharded)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())

	_, tmp := findChild(t, objs, "app-0-tmp")
	g.Expect(tmp.Spec.IngressClassName).To(Equal(testOldShardClass))

	_, main := findChild(t, objs, "app")
	g.Expect(main.Spec.IngressClassName).To(Equal(testOldShardClass))
}

func testOwnerRef() []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{
		APIVersion:         controllerv1.GroupVersion.String(),
		Kind:               "ShardedHTTPProxy",
		Name:               "app",
		Controller:         &yes,
		BlockOwnerDeletion: &yes,
	}}
}

// vhostChild builds an owned child HTTPProxy. An empty fqdn produces a child
// with no virtualHost at all, i.e. a base/root proxy.
func vhostChild(name, fqdn, class string) *contourv1.HTTPProxy {
	child := &contourv1.HTTPProxy{
		TypeMeta: metav1.TypeMeta{Kind: "HTTPProxy", APIVersion: contourv1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "default",
			Labels:          map[string]string{testClassLabel: class},
			OwnerReferences: testOwnerRef(),
		},
		Spec: contourv1.HTTPProxySpec{IngressClassName: class},
	}
	if fqdn != "" {
		child.Spec.VirtualHost = &contourv1.VirtualHost{Fqdn: fqdn}
	}
	return child
}

// liveFqdns maps each fqdn in the cluster to the objects claiming it. Any entry
// with more than one name is a DuplicateVhost.
func liveFqdns(t *testing.T, c client.Client) map[string][]string {
	t.Helper()
	var list contourv1.HTTPProxyList
	if err := c.List(context.Background(), &list, client.InNamespace("default")); err != nil {
		t.Fatal(err)
	}
	byFqdn := map[string][]string{}
	for _, item := range list.Items {
		if item.Spec.VirtualHost == nil || item.Spec.VirtualHost.Fqdn == "" {
			continue
		}
		byFqdn[item.Spec.VirtualHost.Fqdn] = append(byFqdn[item.Spec.VirtualHost.Fqdn], item.Name)
	}
	return byFqdn
}

func generatedNames(objs []NewChildObj) []string {
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.Obj.GetName())
	}
	return names
}

func TestVhostIndexAllocatorReusesIndexOnHostRemoval(t *testing.T) {
	existing := []contourv1.HTTPProxy{
		*vhostChild("app-0", "a", testNewShardClass),
		*vhostChild("app-1", "b", testNewShardClass),
		*vhostChild("app-2", "c", testNewShardClass),
	}

	for _, tc := range []struct {
		name  string
		hosts []string
		want  []int
	}{
		{"middle host removed keeps the others pinned", []string{"a", "c"}, []int{0, 2}},
		{"unchanged list allocates nothing new", []string{"a", "b", "c"}, []int{0, 1, 2}},
		{"appended host takes the first free index", []string{"a", "b", "c", "d"}, []int{0, 1, 2, 3}},
		{"a new host never takes an index a draining child still holds", []string{"x", "a"}, []int{3, 0}},
		{"a repeated host maps to one index", []string{"a", "b", "a"}, []int{0, 1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			alloc := allocatorFor(vhostAllocators(existing), "app")
			got := make([]int, 0, len(tc.hosts))
			for _, h := range tc.hosts {
				got = append(got, alloc.indexFor(h))
			}
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

func TestSplitIndexedName(t *testing.T) {
	g := NewWithT(t)

	for name, want := range map[string]struct {
		prefix string
		idx    int
		ok     bool
	}{
		"app-0":       {"app", 0, true},
		"app-0-12":    {"app-0", 12, true},
		"app-0-tmp":   {"", 0, false},
		"app-0-tmp-3": {"app-0-tmp", 3, true},
		"app":         {"", 0, false},
		"app-007":     {"", 0, false},
		"app--1":      {"app-", 1, true},
		"app-":        {"", 0, false},
		"-0":          {"", 0, false},
	} {
		prefix, idx, ok := splitIndexedName(name)
		g.Expect(ok).To(Equal(want.ok), "ok for %q", name)
		if want.ok {
			g.Expect(prefix).To(Equal(want.prefix), "prefix for %q", name)
			g.Expect(idx).To(Equal(want.idx), "index for %q", name)
		}
	}
}

// Removing a host from the middle of the list must not renumber the hosts after
// it. Positional names shifted them all down one index, which rewrote the fqdn
// of every following child.
func TestNewHTTPProxiesKeepsVhostIndexWhenMiddleHostRemoved(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a,c", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded,
		vhostChild("app", "", testNewShardClass),
		vhostChild("app-0", "a", testNewShardClass),
		vhostChild("app-1", "b", testNewShardClass),
		vhostChild("app-2", "c", testNewShardClass),
	)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(generatedNames(objs)).To(ConsistOf("app", "app-0", "app-2"))

	_, first := findChild(t, objs, "app-0")
	g.Expect(first.Spec.VirtualHost.Fqdn).To(Equal("a"))
	_, last := findChild(t, objs, "app-2")
	g.Expect(last.Spec.VirtualHost.Fqdn).To(Equal("c"))

	seen := map[string]string{}
	for _, o := range objs {
		proxy := o.Obj.(*contourv1.HTTPProxy)
		if proxy.Spec.VirtualHost == nil {
			continue
		}
		fqdn := proxy.Spec.VirtualHost.Fqdn
		g.Expect(seen).NotTo(HaveKey(fqdn), "fqdn %q generated twice", fqdn)
		seen[fqdn] = proxy.Name
	}
}

// The reported outage: after removing one vhost, two HTTPProxies carried the
// same fqdn on the same ingress class and Contour rejected both.
func TestApplyObjectsNeverProducesDuplicateFqdn(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a,c", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded,
		vhostChild("app", "", testNewShardClass),
		vhostChild("app-0", "a", testNewShardClass),
		vhostChild("app-1", "b", testNewShardClass),
		vhostChild("app-2", "c", testNewShardClass),
	)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.applyObjectsToCluster(objs)
	g.Expect(err).NotTo(HaveOccurred())

	for fqdn, owners := range liveFqdns(t, r.Client) {
		g.Expect(owners).To(HaveLen(1), "fqdn %q is claimed by %v", fqdn, owners)
	}

	// The children that keep their host must not be touched at all, and only
	// the child of the removed host may be scheduled for deletion.
	var list contourv1.HTTPProxyList
	g.Expect(r.Client.List(r.ctx, &list, client.InNamespace("default"))).To(Succeed())
	scheduled := []string{}
	for _, item := range list.Items {
		if _, ok := item.Annotations[AutoDeleteAfterAnnotation]; ok {
			scheduled = append(scheduled, item.Name)
		}
	}
	g.Expect(scheduled).To(ConsistOf("app-1"))

	got := &contourv1.HTTPProxy{}
	g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-2"}, got)).To(Succeed())
	g.Expect(got.Spec.VirtualHost.Fqdn).To(Equal("c"))
	g.Expect(got.Annotations).NotTo(HaveKey(AutoDeleteAfterAnnotation))
}

// Building a tree from scratch takes one create per pass. No intermediate state
// may show one fqdn on two objects, and the final layout must be the natural
// one.
func TestNewHTTPProxiesAllocatesStablyAcrossPartialCreation(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a,b,c", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded)

	for pass := 0; pass < 10; pass++ {
		objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
		g.Expect(err).NotTo(HaveOccurred())
		_, err = r.applyObjectsToCluster(objs)
		g.Expect(err).NotTo(HaveOccurred())

		for fqdn, owners := range liveFqdns(t, r.Client) {
			g.Expect(owners).To(HaveLen(1), "pass %d: fqdn %q is claimed by %v", pass, fqdn, owners)
		}
	}

	byFqdn := liveFqdns(t, r.Client)
	g.Expect(byFqdn["a"]).To(Equal([]string{"app-0"}))
	g.Expect(byFqdn["b"]).To(Equal([]string{"app-1"}))
	g.Expect(byFqdn["c"]).To(Equal([]string{"app-2"}))
}

// A leftover main object from another shard layout carries no virtualHost. It
// holds its index without claiming a host, so no live fqdn is ever written onto
// an object that is on its way out.
func TestNewHTTPProxiesSkipsIndexHeldByChildWithoutVirtualHost(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a,b,c,d,e", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded,
		vhostChild("app-3", "", testOldShardClass),
	)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(generatedNames(objs)).To(ConsistOf("app", "app-0", "app-1", "app-2", "app-4", "app-5"))

	untouched := &contourv1.HTTPProxy{}
	g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-3"}, untouched)).To(Succeed())
	g.Expect(untouched.Spec.VirtualHost).To(BeNil())
}

// A host repeated in the annotation must resolve to one child rather than two
// objects racing for the same fqdn.
func TestNewHTTPProxiesDeduplicatesRepeatedHosts(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a,b,a", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())

	byName := map[string]string{}
	for _, o := range objs {
		proxy := o.Obj.(*contourv1.HTTPProxy)
		if proxy.Spec.VirtualHost == nil {
			continue
		}
		if prev, seen := byName[proxy.Name]; seen {
			g.Expect(proxy.Spec.VirtualHost.Fqdn).To(Equal(prev), "%s generated with two fqdns", proxy.Name)
			continue
		}
		byName[proxy.Name] = proxy.Spec.VirtualHost.Fqdn
	}
	g.Expect(byName).To(Equal(map[string]string{"app-0": "a", "app-1": "b"}))
}

// tmp children live under their own name prefix, so they must not make the main
// allocator think an index is taken.
func TestNewHTTPProxiesIgnoresTmpChildrenWhenAllocating(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded,
		vhostChild("app-0-tmp-0", "a", testOldShardClass),
	)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())

	_, main := findChild(t, objs, "app-0")
	g.Expect(main.Spec.VirtualHost.Fqdn).To(Equal("a"))
}

// A cluster that is already carrying a duplicate must be healed on the first
// pass rather than after the multi-period drain: while two objects collide on
// one fqdn Contour serves neither, so draining protects nothing.
func TestDeleteUnlistedRemovesDuplicateFqdnImmediately(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a,c", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded,
		vhostChild("app", "", testNewShardClass),
		vhostChild("app-0", "a", testNewShardClass),
		vhostChild("app-1", "c", testNewShardClass),
		vhostChild("app-2", "c", testNewShardClass),
	)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.applyObjectsToCluster(objs)
	g.Expect(err).NotTo(HaveOccurred())

	gone := &contourv1.HTTPProxy{}
	err = r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-2"}, gone)
	g.Expect(errors.IsNotFound(err)).To(BeTrue(), "the duplicate must be deleted in this pass")

	kept := &contourv1.HTTPProxy{}
	g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-1"}, kept)).To(Succeed())
	g.Expect(kept.Spec.VirtualHost.Fqdn).To(Equal("c"))
	g.Expect(kept.Annotations).NotTo(HaveKey(AutoDeleteAfterAnnotation))

	for fqdn, owners := range liveFqdns(t, r.Client) {
		g.Expect(owners).To(HaveLen(1), "fqdn %q is claimed by %v", fqdn, owners)
	}
}

// During a migration the tmp children mirror the main children's fqdns on
// purpose. They must keep the normal drain even when the main children are
// still on the old class, i.e. when the collision is within one class.
func TestDeleteUnlistedKeepsTmpDuplicateOnDrain(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a,c", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded,
		vhostChild("app", "", testNewShardClass),
		vhostChild("app-0", "a", testNewShardClass),
		vhostChild("app-1", "c", testNewShardClass),
		vhostChild("app-0-tmp-1", "c", testNewShardClass),
	)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.applyObjectsToCluster(objs)
	g.Expect(err).NotTo(HaveOccurred())

	tmp := &contourv1.HTTPProxy{}
	g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-0-tmp-1"}, tmp)).
		To(Succeed(), "tmp children must not be short-circuited")
	g.Expect(tmp.Annotations).To(HaveKey(AutoDeleteAfterAnnotation))
}

// A stray on a different ingress class is not a DuplicateVhost - two Contour
// instances each see one proxy - so it keeps the normal drain.
func TestDeleteUnlistedKeepsDuplicateOnAnotherClassOnDrain(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a,c", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded,
		vhostChild("app", "", testNewShardClass),
		vhostChild("app-0", "a", testNewShardClass),
		vhostChild("app-1", "c", testNewShardClass),
		vhostChild("app-9", "c", testOldShardClass),
	)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.applyObjectsToCluster(objs)
	g.Expect(err).NotTo(HaveOccurred())

	stray := &contourv1.HTTPProxy{}
	g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-9"}, stray)).
		To(Succeed(), "a cross-class duplicate must not be short-circuited")
	g.Expect(stray.Annotations).To(HaveKey(AutoDeleteAfterAnnotation))
}

// Inserting a host in the middle used to shift every later host up one index.
// Because applyObjectsToCluster returns after the first create, the host
// shifted off the end then had no proxy at all for the rest of the pass.
func TestApplyObjectsInsertingHostLeavesExistingChildrenAlone(t *testing.T) {
	g := NewWithT(t)

	sharded := newRegularModeShardedHTTPProxy("a,b,c", nil)
	r := newTestShardedHTTPProxyReconciler(t, sharded,
		vhostChild("app", "", testNewShardClass),
		vhostChild("app-0", "a", testNewShardClass),
		vhostChild("app-1", "c", testNewShardClass),
	)

	// Every host must stay reachable through the whole convergence.
	for pass := 0; pass < 5; pass++ {
		objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
		g.Expect(err).NotTo(HaveOccurred())
		_, err = r.applyObjectsToCluster(objs)
		g.Expect(err).NotTo(HaveOccurred())

		byFqdn := liveFqdns(t, r.Client)
		g.Expect(byFqdn["a"]).To(Equal([]string{"app-0"}), "pass %d", pass)
		g.Expect(byFqdn["c"]).To(Equal([]string{"app-1"}), "pass %d", pass)
	}

	g.Expect(liveFqdns(t, r.Client)["b"]).To(Equal([]string{"app-2"}))
}
