package controller

import (
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
)

// applyRateLimit used to index NextApplyTime directly. The map is only seeded
// by CheckClusterShards, which runs once at operator startup, so the lookup
// returned a nil *applyPlan for any shard it had not seen and dereferencing it
// panicked the reconcile worker. Here the object is being migrated to an
// ingress class the operator did not know about at startup, so the shard it is
// applied to has no plan yet.
func TestApplyRateLimitSeedsPlanForUnknownShard(t *testing.T) {
	g := NewWithT(t)

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy())
	g.Expect(r.NextApplyTime).NotTo(HaveKey(testNewShardClass))

	res, err := r.applyRateLimit(r.objKey, logr.Discard())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0))
	g.Expect(r.NextApplyTime).To(HaveKey(testNewShardClass))
}

// The same nil plan is reached from the deleting branch, where the shard comes
// from the object's own status rather than from the current shard list: after
// a class migration the status still records children under a shard of the old
// class, which no longer has a plan of its own.
func TestApplyRateLimitSeedsPlanForStaleStatusShard(t *testing.T) {
	g := NewWithT(t)

	// Both shards in the status and only the new one in the shard list, so the
	// stale old shard is what the rate limiter schedules the deletion on.
	sharded := newMigratingShardedHTTPProxy()
	sharded.Status.CreatedObjects[testNewShardClass] = []map[string]string{{"kind": "HTTPProxy", "name": "app-0"}}

	r := newTestShardedHTTPProxyReconciler(t, sharded)
	r.addKey(r.objKey, r.ManagedList)
	g.Expect(r.NextApplyTime).NotTo(HaveKey(testOldShardClass))

	res, err := r.applyRateLimit(r.objKey, logr.Discard())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0))
	g.Expect(r.NextApplyTime).To(HaveKey(testOldShardClass))
}

// setShardInfo backfills plans for the shards about to be applied to, so an
// ingress class created after startup no longer depends on an operator restart.
func TestSetShardInfoSeedsApplyPlans(t *testing.T) {
	g := NewWithT(t)

	r := newTestShardedHTTPProxyReconciler(t, newMigratingShardedHTTPProxy())
	r.MaxShards = map[string]int{"new-class": 1}

	g.Expect(r.setShardInfo(logr.Discard())).To(Succeed())

	g.Expect(r.Shards).To(HaveLen(1))
	g.Expect(r.NextApplyTime).To(HaveKey(r.Shards[0].ShardName))
}
