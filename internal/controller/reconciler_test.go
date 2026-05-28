package controller

import (
	"testing"
	"time"

	"github.com/go-logr/logr"

	controllerv1 "k8s.tochka.com/sharded-ingress-controller/api/v1"
)

func TestApplyRateLimitInitializesMissingApplyPlan(t *testing.T) {
	terminationPeriod := 10 * time.Minute
	shardUpdateCooldown := time.Minute
	createdObjects := map[string][]map[string]string{
		"vpn-0": {
			{"kind": "Ingress", "name": "app-0"},
		},
	}

	r := &ShardedReconciler{
		TerminationPeriod:   &terminationPeriod,
		ShardUpdateCooldown: &shardUpdateCooldown,
		WaitingList:         map[string]bool{},
		ReadyList:           map[string]bool{},
		ManagedList:         map[string]bool{},
		ErrorList:           map[string]bool{},
		NextApplyTime:       map[string]*applyPlan{},
		ShardedObject: &controllerv1.ShardedIngress{
			Status: controllerv1.ShardedStatus{
				CreatedObjects: createdObjects,
			},
		},
		Shards: []Shards{
			{ShardNumber: 0, ShardName: "vpn"},
		},
		ctrlName: "shardedingress",
	}

	result, err := r.applyRateLimit("default/app", logr.Discard())
	if err != nil {
		t.Fatalf("applyRateLimit returned error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("expected positive requeue delay, got %s", result.RequeueAfter)
	}
	if r.NextApplyTime["vpn"] == nil {
		t.Fatal("expected missing apply plan to be initialized")
	}
}
