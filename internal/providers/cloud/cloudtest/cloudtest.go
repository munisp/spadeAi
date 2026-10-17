// Package cloudtest is the shared contract every cloud driver must pass against its fake API.
package cloudtest

import (
	"context"
	"strings"
	"testing"

	"github.com/munisp/spadeai/internal/providers/cloud"
)

// Contract checks idempotent create per attempt, reconciliation, delete-all, and failing closed.
// good uses a valid token, bad a rejected one, and unset none.
func Contract(t *testing.T, good, bad, unset cloud.Driver) {
	t.Helper()
	ContractSize(t, good.Name()+"-size", good, bad, unset)
}

// ContractSize is Contract for drivers that parse the size code.
func ContractSize(t *testing.T, size string, good, bad, unset cloud.Driver) {
	t.Helper()
	ctx := context.Background()
	o := cloud.CreateOpts{ServiceID: "0123456789abcdef-svc", Attempt: "a1b2c3d4", Size: size, UserData: cloud.CloudInit(cloud.Boot{Run: "echo hi"}), Script: cloud.Script(cloud.Boot{Run: "echo hi"})}
	other := cloud.CreateOpts{ServiceID: "0123456789abcdef-other", Attempt: "ffff0000", Size: o.Size, UserData: o.UserData, Script: o.Script}
	if _, _, err := cloud.Ensure(ctx, good, other); err != nil {
		t.Fatalf("other service: %v", err)
	}
	s1, created, err := cloud.Ensure(ctx, good, o)
	if err != nil || !created || s1.ID == "" || s1.Attempt != o.Attempt {
		t.Fatalf("first ensure: %+v %v %v", s1, created, err)
	}
	s2, created, err := cloud.Ensure(ctx, good, o)
	if err != nil || created || s2.ID != s1.ID {
		t.Fatalf("retry must reuse the server: %+v %v %v", s2, created, err)
	}
	o2 := o
	o2.Attempt = "e5f6a7b8"
	if s3, created, err := cloud.Ensure(ctx, good, o2); err != nil || !created || s3.ID == s1.ID {
		t.Fatalf("new attempt must get a new server: %+v %v %v", s3, created, err)
	}
	if err := cloud.DeleteOthers(ctx, good, o.ServiceID, o2.Attempt); err != nil {
		t.Fatalf("delete others: %v", err)
	}
	if left, _ := good.List(ctx, o.ServiceID); len(left) != 1 || left[0].Attempt != o2.Attempt {
		t.Fatalf("only the current attempt's server may survive reconciliation: %+v", left)
	}
	if ok, err := cloud.DeleteAll(ctx, good, o.ServiceID); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, err := cloud.DeleteAll(ctx, good, o.ServiceID); err != nil || ok {
		t.Fatalf("second delete must be a no-op: %v %v", ok, err)
	}
	if left, _ := good.List(ctx, other.ServiceID); len(left) != 1 {
		t.Fatalf("another service's server must be untouched, %d left", len(left))
	}
	if err := good.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if unset.Configured() {
		t.Fatal("a driver without a token must report unconfigured")
	}
	if err := unset.Ping(ctx); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing token must fail: %v", err)
	}
	if _, _, err := cloud.Ensure(ctx, bad, o); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token must fail: %v", err)
	}
}
