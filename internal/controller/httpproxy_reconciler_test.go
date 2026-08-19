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

// testOwnerRef matches what SetControllerReference produces for the "app"
// ShardedHTTPProxy, so fixture children are recognized by getObjectChildren.
func testOwnerRef() []metav1.OwnerReference {
	isController := true
	blockOwnerDeletion := true
	return []metav1.OwnerReference{{
		APIVersion:         controllerv1.GroupVersion.String(),
		Kind:               "ShardedHTTPProxy",
		Name:               "app",
		Controller:         &isController,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}}
}

// newOldShardChild returns a child HTTPProxy that still carries the old shard
// class, i.e. a leftover from before the class migration.
func newOldShardChild(name string) *contourv1.HTTPProxy {
	return &contourv1.HTTPProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				testClassLabel: testOldShardClass,
				testRootLabel:  "true",
			},
			OwnerReferences: testOwnerRef(),
		},
		Spec: contourv1.HTTPProxySpec{IngressClassName: testOldShardClass},
	}
}

// newMigratingShardedHTTPProxyWithHosts adds a virtual host alias so the child
// tree is a root proxy plus one proxy per host, and the tmp copy has to mirror
// the whole tree.
func newMigratingShardedHTTPProxyWithHosts() *controllerv1.ShardedHTTPProxy {
	sharded := newMigratingShardedHTTPProxy()
	sharded.Annotations = map[string]string{testVHAnnotation: "alias.example.com"}
	return sharded
}

// settle runs the generate/apply cycle several times. applyObjectsToCluster
// performs at most one create or delete per pass, so a whole tree needs a few
// passes to converge.
func settle(t *testing.T, r *ShardedHTTPProxyReconciler, passes int) {
	t.Helper()
	for i := 0; i < passes; i++ {
		objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
		if err != nil {
			t.Fatalf("pass %d: generate children: %v", i, err)
		}
		if _, err := r.applyObjectsToCluster(objs); err != nil {
			t.Fatalf("pass %d: apply children: %v", i, err)
		}
	}
}

// advance simulates elapsed wall clock by moving every auto-delete-after
// deadline closer by delta.
func advance(t *testing.T, r *ShardedHTTPProxyReconciler, delta time.Duration) {
	t.Helper()
	list := &contourv1.HTTPProxyList{}
	if err := r.Client.List(r.ctx, list); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		raw, ok := obj.Annotations[AutoDeleteAfterAnnotation]
		if !ok {
			continue
		}
		deadline, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatal(err)
		}
		obj.Annotations[AutoDeleteAfterAnnotation] = deadline.Add(-delta).UTC().Format(time.RFC3339)
		if err := r.Client.Update(r.ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
}

func getChild(t *testing.T, r *ShardedHTTPProxyReconciler, name string) *contourv1.HTTPProxy {
	t.Helper()
	obj := &contourv1.HTTPProxy{}
	if err := r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: name}, obj); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return obj
}

func childNames(t *testing.T, r *ShardedHTTPProxyReconciler) []string {
	t.Helper()
	list := &contourv1.HTTPProxyList{}
	if err := r.Client.List(r.ctx, list); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	return names
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

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy(), newOldShardChild("app-0"))
	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(2))

	tmpChild, tmp := findChild(t, objs, "app-0-tmp")
	g.Expect(tmp.Spec.IngressClassName).To(Equal(testOldShardClass))
	g.Expect(tmp.Labels).To(HaveKeyWithValue(testClassLabel, testOldShardClass))
	g.Expect(tmp.Labels).To(HaveKeyWithValue(testRootLabel, "true"))
	g.Expect(tmp.Annotations).To(HaveKeyWithValue("old-shard", testOldShardClass))
	g.Expect(tmpChild.ShardName).To(Equal(testNewShardClass))

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
			Name:            "app-0-tmp",
			Namespace:       "default",
			Annotations:     map[string]string{"old-shard": testOldShardClass},
			OwnerReferences: testOwnerRef(),
		},
	}
	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy(), tmp, newOldShardChild("app-0"))

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))

	mainChild, main := findChild(t, objs, "app-0")
	g.Expect(main.Spec.IngressClassName).To(Equal(testOldShardClass))
	g.Expect(main.Labels).To(HaveKeyWithValue(testClassLabel, testOldShardClass))
	// Even while the spec keeps the old class, status bookkeeping must stay on
	// the new shard, otherwise deleteUnlistedObjects schedules the live main
	// object for deletion.
	g.Expect(mainChild.ShardName).To(Equal(testNewShardClass))
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

// A resharding conflict is only real while the old child object still exists
// in the cluster with a class other than the new shard. Status entries can
// outlive their objects, and a conflict derived from such a stale entry used
// to re-create the tmp object in an endless cycle.
func TestCheckReshardingConflictRequiresLiveOldChild(t *testing.T) {
	g := NewWithT(t)

	// Old child still exists with the old class: the conflict is real.
	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy(), newOldShardChild("app-0"))
	g.Expect(r.CheckReshardingConflict(testNewShardClass, "app-0")).To(Equal(testOldShardClass))

	// The old child is gone but its status entry survived: no conflict.
	r = newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy())
	g.Expect(r.CheckReshardingConflict(testNewShardClass, "app-0")).To(Equal(""))

	// The child already switched to the new class: migration is done.
	migrated := newOldShardChild("app-0")
	migrated.Labels[testClassLabel] = testNewShardClass
	migrated.Spec.IngressClassName = testNewShardClass
	r = newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy(), migrated)
	g.Expect(r.CheckReshardingConflict(testNewShardClass, "app-0")).To(Equal(""))
}

// After the tmp object and the old child are gone, a stale status entry under
// the old shard must not resurrect the tmp object.
func TestNewHTTPProxiesStaleStatusDoesNotRecreateTmp(t *testing.T) {
	g := NewWithT(t)

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy())

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))

	mainChild, main := findChild(t, objs, "app-0")
	g.Expect(main.Spec.IngressClassName).To(Equal(testNewShardClass))
	g.Expect(mainChild.ShardName).To(Equal(testNewShardClass))
	for _, o := range objs {
		g.Expect(o.Obj.GetName()).NotTo(HaveSuffix("tmp"))
	}
}

// Marking the tmp object for deletion extends auto-delete-after, which used to
// reopen the migration window and flap the main children back to the old
// class. The unregister annotation must close the window for good.
func TestCheckTmpObjAnnotationsClosedOnceUnregistered(t *testing.T) {
	g := NewWithT(t)

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy())

	annotations := map[string]string{
		"old-shard":               testOldShardClass,
		AutoDeleteAfterAnnotation: time.Now().Add(3 * time.Minute).UTC().Format(time.RFC3339),
	}
	oldClass, ok := r.checkTmpObjAnnotations(annotations)
	g.Expect(ok).To(BeTrue())
	g.Expect(oldClass).To(Equal(testOldShardClass))

	annotations[testUnregisterAnnotation] = "true"
	_, ok = r.checkTmpObjAnnotations(annotations)
	g.Expect(ok).To(BeFalse())
}

// Mid-migration the live main object must never receive the auto-delete-after
// annotation: it used to be registered in the status list under the old shard,
// so every reconcile scheduled it for deletion and the next one wiped the
// annotation again, looping forever without ever finishing the migration.
func TestApplyObjectsMigrationDoesNotChurnAutoDeleteOnMain(t *testing.T) {
	g := NewWithT(t)

	tmp := &contourv1.HTTPProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "app-0-tmp",
			Namespace:       "default",
			Annotations:     map[string]string{"old-shard": testOldShardClass},
			OwnerReferences: testOwnerRef(),
		},
		Spec: contourv1.HTTPProxySpec{IngressClassName: testOldShardClass},
	}
	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy(), tmp, newOldShardChild("app-0"))

	var tmpDeleteAfter string
	for cycle := 1; cycle <= 3; cycle++ {
		objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
		g.Expect(err).NotTo(HaveOccurred())
		_, err = r.applyObjectsToCluster(objs)
		g.Expect(err).NotTo(HaveOccurred())

		main := &contourv1.HTTPProxy{}
		g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-0"}, main)).To(Succeed())
		g.Expect(main.Annotations).NotTo(HaveKey(AutoDeleteAfterAnnotation),
			"cycle %d: live main object must not be scheduled for deletion", cycle)

		gotTmp := &contourv1.HTTPProxy{}
		g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app-0-tmp"}, gotTmp)).To(Succeed())
		// The deletion flow itself must stay active: the tmp object gets its
		// auto-delete-after on the first pass and the timestamp must not be
		// re-created afterwards.
		g.Expect(gotTmp.Annotations).To(HaveKey(AutoDeleteAfterAnnotation), "cycle %d", cycle)
		if tmpDeleteAfter == "" {
			tmpDeleteAfter = gotTmp.Annotations[AutoDeleteAfterAnnotation]
		} else {
			g.Expect(gotTmp.Annotations[AutoDeleteAfterAnnotation]).To(Equal(tmpDeleteAfter),
				"cycle %d: tmp auto-delete-after must not be rescheduled", cycle)
		}
	}
}

// Step 1 of the migration flow: the tmp copy must mirror the whole child tree
// (root proxy plus one proxy per virtual host) on the old shard, so both trees
// take traffic while DNS/service discovery switch over. Per-host tmp objects
// used to never be created at all.
func TestNewHTTPProxiesMigrationCreatesFullTmpTree(t *testing.T) {
	g := NewWithT(t)

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxyWithHosts(), newOldShardChild("app-0"))
	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())

	tmpRootChild, tmpRoot := findChild(t, objs, "app-0-tmp")
	g.Expect(tmpRoot.Spec.IngressClassName).To(Equal(testOldShardClass))
	g.Expect(tmpRoot.Labels).To(HaveKeyWithValue(testRootLabel, "true"))
	g.Expect(tmpRoot.Annotations).To(HaveKeyWithValue("old-shard", testOldShardClass))
	g.Expect(tmpRootChild.ShardName).To(Equal(testNewShardClass))

	tmpHostChild, tmpHost := findChild(t, objs, "app-0-tmp-0")
	g.Expect(tmpHost.Spec.IngressClassName).To(Equal(testOldShardClass))
	g.Expect(tmpHost.Labels).To(HaveKeyWithValue(testClassLabel, testOldShardClass))
	g.Expect(tmpHost.Annotations).To(HaveKeyWithValue("old-shard", testOldShardClass))
	g.Expect(tmpHost.Spec.VirtualHost.Fqdn).To(Equal("alias.example.com"))
	// The per-host tmp proxy delegates to the tmp root, not to the main tree.
	g.Expect(tmpHost.Spec.Includes).To(HaveLen(1))
	g.Expect(tmpHost.Spec.Includes[0].Name).To(Equal("app-0-tmp"))
	g.Expect(tmpHostChild.ShardName).To(Equal(testNewShardClass))

	// The main tree keeps the old class for now and never carries old-shard.
	_, main := findChild(t, objs, "app-0")
	g.Expect(main.Spec.IngressClassName).To(Equal(testOldShardClass))
	g.Expect(main.Annotations).NotTo(HaveKey("old-shard"))
	_, mainHost := findChild(t, objs, "app-0-0")
	g.Expect(mainHost.Spec.IngressClassName).To(Equal(testOldShardClass))
	g.Expect(mainHost.Spec.Includes[0].Name).To(Equal("app-0"))
}

// Existing tmp objects must be left out of the generated list: regenerating
// them would reconcile away the auto-delete-after annotation that drives their
// unregister/delete timeline.
func TestNewHTTPProxiesTmpTreeNotRegeneratedWhenPresent(t *testing.T) {
	g := NewWithT(t)

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxyWithHosts(), newOldShardChild("app-0"))
	settle(t, r, 6)

	g.Expect(childNames(t, r)).To(ConsistOf("app-0", "app-0-0", "app-0-tmp", "app-0-tmp-0"))

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	for _, o := range objs {
		g.Expect(o.Obj.GetName()).NotTo(ContainSubstring("tmp"))
	}
}

// The whole migration timeline, one phase per step:
//
//	t0        tmp tree created on the old shard, auto-delete-after = t0+3*TP
//	t0+TP     main tree moves to the new shard, tmp keeps the old one
//	t0+2*TP   tmp tree unregistered, auto-delete-after pushed to t0+5*TP
//	t0+5*TP   tmp tree deleted and the stale status entries swept
func TestMigrationFlowFollowsTerminationTimeline(t *testing.T) {
	g := NewWithT(t)

	terminationPeriod := time.Minute
	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxyWithHosts(), newOldShardChild("app-0"))

	// t0: both trees live on the old shard.
	settle(t, r, 6)
	g.Expect(childNames(t, r)).To(ConsistOf("app-0", "app-0-0", "app-0-tmp", "app-0-tmp-0"))
	for _, name := range []string{"app-0", "app-0-0", "app-0-tmp", "app-0-tmp-0"} {
		child := getChild(t, r, name)
		g.Expect(child.Spec.IngressClassName).To(Equal(testOldShardClass), name)
		g.Expect(child.Labels).To(HaveKeyWithValue(testClassLabel, testOldShardClass), name)
	}
	for _, name := range []string{"app-0-tmp", "app-0-tmp-0"} {
		tmp := getChild(t, r, name)
		g.Expect(tmp.Annotations).To(HaveKey(AutoDeleteAfterAnnotation), name)
		g.Expect(tmp.Annotations).NotTo(HaveKeyWithValue(testUnregisterAnnotation, "true"), name)
		deadline, err := time.Parse(time.RFC3339, tmp.Annotations[AutoDeleteAfterAnnotation])
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(deadline).To(BeTemporally("~", time.Now().Add(3*terminationPeriod), 30*time.Second), name)
	}
	// The live main tree is never scheduled for deletion.
	for _, name := range []string{"app-0", "app-0-0"} {
		g.Expect(getChild(t, r, name).Annotations).NotTo(HaveKey(AutoDeleteAfterAnnotation), name)
	}

	// t0+TP: the main tree switches to the new shard, the tmp tree holds the old one.
	advance(t, r, terminationPeriod)
	settle(t, r, 4)
	for _, name := range []string{"app-0", "app-0-0"} {
		main := getChild(t, r, name)
		g.Expect(main.Spec.IngressClassName).To(Equal(testNewShardClass), name)
		g.Expect(main.Labels).To(HaveKeyWithValue(testClassLabel, testNewShardClass), name)
		g.Expect(main.Annotations).NotTo(HaveKey(AutoDeleteAfterAnnotation), name)
	}
	for _, name := range []string{"app-0-tmp", "app-0-tmp-0"} {
		tmp := getChild(t, r, name)
		g.Expect(tmp.Spec.IngressClassName).To(Equal(testOldShardClass), name)
		g.Expect(tmp.Annotations).NotTo(HaveKeyWithValue(testUnregisterAnnotation, "true"), name)
	}

	// t0+2*TP: the tmp tree is unregistered and its deadline moves to t0+5*TP.
	advance(t, r, terminationPeriod)
	settle(t, r, 4)
	for _, name := range []string{"app-0-tmp", "app-0-tmp-0"} {
		tmp := getChild(t, r, name)
		g.Expect(tmp.Annotations).To(HaveKeyWithValue(testUnregisterAnnotation, "true"), name)
		deadline, err := time.Parse(time.RFC3339, tmp.Annotations[AutoDeleteAfterAnnotation])
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(deadline).To(BeTemporally("~", time.Now().Add(3*terminationPeriod), 30*time.Second), name)
	}
	// Pushing the deadline out must not reopen the migration window.
	for _, name := range []string{"app-0", "app-0-0"} {
		g.Expect(getChild(t, r, name).Spec.IngressClassName).To(Equal(testNewShardClass), name)
	}

	// t0+5*TP: the tmp tree is deleted and the stale status entries are swept.
	advance(t, r, 3*terminationPeriod)
	settle(t, r, 6)
	g.Expect(childNames(t, r)).To(ConsistOf("app-0", "app-0-0"))

	refreshed := &controllerv1.ShardedHTTPProxy{}
	g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app"}, refreshed)).To(Succeed())
	g.Expect(refreshed.Status.CreatedObjects).To(HaveKey(testNewShardClass))
	g.Expect(refreshed.Status.CreatedObjects).NotTo(HaveKey(testOldShardClass))
	names := []string{}
	for _, entry := range refreshed.Status.CreatedObjects[testNewShardClass] {
		names = append(names, entry["name"])
	}
	g.Expect(names).To(ConsistOf("app-0", "app-0-0"))

	// The migration is over: a further reconcile must be a no-op, not a new
	// tmp tree.
	settle(t, r, 3)
	g.Expect(childNames(t, r)).To(ConsistOf("app-0", "app-0-0"))
}

// Once a same-name migration finishes, the stale status entry under the old
// shard must be swept away, otherwise the rate limiter keeps planning
// deletions forever.
func TestDeleteUnlistedSweepsStaleStatusEntries(t *testing.T) {
	g := NewWithT(t)

	sharded := newMigratingShardedHTTPProxy()
	sharded.Status.CreatedObjects = map[string][]map[string]string{
		testOldShardClass: {{"kind": "HTTPProxy", "name": "app-0"}},
		testNewShardClass: {{"kind": "HTTPProxy", "name": "app-0"}},
	}
	migrated := newOldShardChild("app-0")
	migrated.Labels[testClassLabel] = testNewShardClass
	migrated.Spec.IngressClassName = testNewShardClass
	r := newTestShardedHTTPProxyReconciler(t, sharded, migrated)

	objs, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	g.Expect(err).NotTo(HaveOccurred())
	for _, o := range objs {
		g.Expect(o.Obj.GetName()).NotTo(HaveSuffix("tmp"))
	}
	_, err = r.applyObjectsToCluster(objs)
	g.Expect(err).NotTo(HaveOccurred())

	refreshed := &controllerv1.ShardedHTTPProxy{}
	g.Expect(r.Client.Get(r.ctx, types.NamespacedName{Namespace: "default", Name: "app"}, refreshed)).To(Succeed())
	g.Expect(refreshed.Status.CreatedObjects).NotTo(HaveKey(testOldShardClass))
	g.Expect(refreshed.Status.CreatedObjects).To(HaveKey(testNewShardClass))
}
