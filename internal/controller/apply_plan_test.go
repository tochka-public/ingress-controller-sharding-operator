package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A plan created on demand must start cold. Stamping it with the current time
// reads as "this shard was just applied to", so the first object to reach a
// shard nothing has been applied to yet pays a full shardUpdateCooldown before
// anything is created.
func TestApplyPlanForCreatesColdPlan(t *testing.T) {
	g := NewWithT(t)

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy())

	ap := r.applyPlanFor("class-with-no-plan")

	g.Expect(ap.lastCreating.IsZero()).To(BeTrue())
	g.Expect(r.NextApplyTime).To(HaveKey("class-with-no-plan"))
}

// applyRateLimit hands out apply slots by moving the shard clock one
// shardUpdateCooldown into the future per object and never moves it back, so a
// shard every object of a class shares accumulates a queue as long as the
// object count. Past the horizon that queue stops being rate limiting and
// becomes an outage: the tail is scheduled hours out and updates never land.
func TestApplyPlanForBoundsTheBacklog(t *testing.T) {
	g := NewWithT(t)

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy())
	backlog := time.Minute
	r.MaxApplyBacklog = &backlog
	r.NextApplyTime["busy"] = &applyPlan{
		lastCreating: time.Now().Add(time.Hour),
		lastDeleting: time.Now().Add(time.Hour),
	}

	ap := r.applyPlanFor("busy")

	g.Expect(time.Until(ap.lastCreating)).To(BeNumerically("<=", backlog))
	g.Expect(time.Until(ap.lastDeleting)).To(BeNumerically("<=", backlog))
}

// A class configured with 0 shards is applied to under its bare name, so
// CheckClusterShards has to seed a plan for that name. The class-0..class-N
// loop runs zero times for it, and the bare name is only seeded on the branch
// taken when the class has no sharded ingress classes in the cluster at all.
func TestCheckClusterShardsSeedsZeroShardClass(t *testing.T) {
	g := NewWithT(t)

	testScheme := runtime.NewScheme()
	g.Expect(networkingv1.AddToScheme(testScheme)).To(Succeed())

	classes := make([]client.Object, 0, 3)
	for _, name := range []string{"vpn-0", "vpn-1", "vpn-2"} {
		classes = append(classes, &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}

	r := &ShardedReconciler{
		Client:    fake.NewClientBuilder().WithScheme(testScheme).WithObjects(classes...).Build(),
		MaxShards: map[string]int{"vpn": 0},
		ctx:       context.Background(),
		ctrlName:  "shardedhttpproxy",
	}
	r.initializeCache()

	g.Expect(r.CheckClusterShards()).To(Succeed())

	g.Expect(r.NextApplyTime).To(HaveKey("vpn"))
}
