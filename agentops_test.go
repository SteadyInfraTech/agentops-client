package agentops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// validRequest is a test helper.
func validRequest() ExposeRequest {
	return ExposeRequest{
		Namespace: "agents", Name: "s1", PoolName: "pool",
		ZoneID: "zone", Subdomain: "s1", AllowedEmails: []string{"a@example.com"},
	}
}

// newClient returns a fake client that assigns UIDs on Create, as a real API
// server does, and optionally fails creating tunnels.
func newClient(failTunnel bool) client.Client {
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if failTunnel && obj.GetObjectKind().GroupVersionKind().Kind == TunnelGVK.Kind {
				return errors.New("boom")
			}
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("uid-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
}

// get is a test helper.
func get(t *testing.T, c client.Client, gvk interface{ String() string }, u *unstructured.Unstructured) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		t.Fatalf("get %s: %v", gvk, err)
	}
}

// TestValidate checks the behaviour its name describes.
func TestValidate(t *testing.T) {
	cases := map[string]func(*ExposeRequest){
		"no namespace":      func(r *ExposeRequest) { r.Namespace = "" },
		"bad name":          func(r *ExposeRequest) { r.Name = "Bad_Name" },
		"neither target":    func(r *ExposeRequest) { r.PoolName = "" },
		"both targets":      func(r *ExposeRequest) { r.AgentServiceName = "svc" },
		"no zone":           func(r *ExposeRequest) { r.ZoneID = "" },
		"dotted subdomain":  func(r *ExposeRequest) { r.Subdomain = "a.b" },
		"no access":         func(r *ExposeRequest) { r.AllowedEmails = nil },
		"public and emails": func(r *ExposeRequest) { r.AllowPublic = true },
		"negative ttl":      func(r *ExposeRequest) { r.TTL = -1 },
		"negative duration": func(r *ExposeRequest) { d := int64(-1); r.MaxDurationSeconds = &d },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := validRequest()
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if err := validRequest().Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

// TestExposeCreatesSessionAndOwnedTunnel checks the behaviour its name describes.
func TestExposeCreatesSessionAndOwnedTunnel(t *testing.T) {
	c := newClient(false)
	d := int64(0)
	req := validRequest()
	req.UserID, req.Parameters, req.MaxDurationSeconds = "u1", map[string]string{"k": "v"}, &d
	req.TTL = 2 * time.Hour

	ex, err := Expose(context.Background(), c, req)
	if err != nil {
		t.Fatal(err)
	}
	if ex.SessionUID != "uid-s1" {
		t.Fatalf("uid = %q", ex.SessionUID)
	}

	session := newObject(SessionGVK, "agents", "s1")
	get(t, c, SessionGVK, session)
	if n, _, _ := unstructured.NestedString(session.Object, "spec", "poolRef", "name"); n != "pool" {
		t.Fatalf("poolRef = %q", n)
	}
	if v, found, _ := unstructured.NestedInt64(session.Object, "spec", "maxDurationSeconds"); !found || v != 0 {
		t.Fatalf("maxDurationSeconds not kept as 0: %v %v", v, found)
	}

	tunnel := newObject(TunnelGVK, "agents", "s1")
	get(t, c, TunnelGVK, tunnel)
	owners := tunnel.GetOwnerReferences()
	if len(owners) != 1 || owners[0].UID != "uid-s1" || owners[0].Kind != "AgentSession" {
		t.Fatalf("owner refs = %+v", owners)
	}
	if owners[0].Controller != nil || owners[0].BlockOwnerDeletion != nil {
		t.Fatalf("owner must be non-controller and non-blocking: %+v", owners[0])
	}
	if n, _, _ := unstructured.NestedString(tunnel.Object, "spec", "sessionRef", "name"); n != "s1" {
		t.Fatalf("sessionRef = %q", n)
	}
	if ttl, _, _ := unstructured.NestedString(tunnel.Object, "spec", "ttl"); ttl != "2h0m0s" {
		t.Fatalf("ttl = %q", ttl)
	}
	if _, found, _ := unstructured.NestedBool(tunnel.Object, "spec", "security", "allowPublic"); found {
		t.Fatal("allowPublic must be omitted when emails are given")
	}
}

// TestExposeAgentServiceAndPublic checks the behaviour its name describes.
func TestExposeAgentServiceAndPublic(t *testing.T) {
	c := newClient(false)
	req := validRequest()
	req.PoolName, req.AgentServiceName = "", "svc"
	req.AllowedEmails, req.AllowPublic = nil, true
	if _, err := Expose(context.Background(), c, req); err != nil {
		t.Fatal(err)
	}
	session := newObject(SessionGVK, "agents", "s1")
	get(t, c, SessionGVK, session)
	if n, _, _ := unstructured.NestedString(session.Object, "spec", "agentServiceRef", "name"); n != "svc" {
		t.Fatalf("agentServiceRef = %q", n)
	}
	tunnel := newObject(TunnelGVK, "agents", "s1")
	get(t, c, TunnelGVK, tunnel)
	if p, _, _ := unstructured.NestedBool(tunnel.Object, "spec", "security", "allowPublic"); !p {
		t.Fatal("allowPublic not set")
	}
}

// TestExposeRemovesSessionWhenTunnelFails checks the behaviour its name describes.
func TestExposeRemovesSessionWhenTunnelFails(t *testing.T) {
	c := newClient(true)
	_, err := Expose(context.Background(), c, validRequest())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	session := newObject(SessionGVK, "agents", "s1")
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(session), session); !apierrors.IsNotFound(err) {
		t.Fatalf("session should be gone, got %v", err)
	}
}

// TestExposeRejectsInvalidRequest checks the behaviour its name describes.
func TestExposeRejectsInvalidRequest(t *testing.T) {
	r := validRequest()
	r.PoolName = ""
	if _, err := Expose(context.Background(), newClient(false), r); err == nil {
		t.Fatal("expected validation error")
	}
}

// TestComplete checks the behaviour its name describes.
func TestComplete(t *testing.T) {
	c := newClient(false)
	ex, err := Expose(context.Background(), c, validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := Complete(context.Background(), c, ex); err != nil {
		t.Fatal(err)
	}
	session := newObject(SessionGVK, "agents", "s1")
	get(t, c, SessionGVK, session)
	if done, _, _ := unstructured.NestedBool(session.Object, "spec", "completed"); !done {
		t.Fatal("completed not set")
	}
	if n, _, _ := unstructured.NestedString(session.Object, "spec", "poolRef", "name"); n != "pool" {
		t.Fatal("merge patch dropped other spec fields")
	}
}

// setStatus is a test helper.
func setStatus(t *testing.T, c client.Client, ex *Exposure, status map[string]interface{}) {
	t.Helper()
	tunnel := newObject(TunnelGVK, ex.Namespace, ex.TunnelName)
	get(t, c, TunnelGVK, tunnel)
	tunnel.Object["status"] = status
	if err := c.Update(context.Background(), tunnel); err != nil {
		t.Fatal(err)
	}
}

// TestWait checks the behaviour its name describes.
func TestWait(t *testing.T) {
	opts := WaitOptions{Interval: time.Millisecond, Timeout: 50 * time.Millisecond}
	cases := []struct {
		name    string
		status  map[string]interface{}
		wantURL string
		wantErr string
		is      error
	}{
		{name: "active and connected",
			status:  map[string]interface{}{"phase": "Active", "connectorReady": true, "hostname": "s1.example.com"},
			wantURL: "https://s1.example.com"},
		{name: "hostname while pending is not enough",
			status:  map[string]interface{}{"phase": "Pending", "hostname": "s1.example.com", "message": "waiting"},
			wantErr: "waiting"},
		{name: "active but connector not ready",
			status:  map[string]interface{}{"phase": "Active", "connectorReady": false, "hostname": "s1.example.com"},
			wantErr: "connector ready false"},
		{name: "failed is retried until timeout",
			status:  map[string]interface{}{"phase": "Failed", "message": "cloudflare 500"},
			wantErr: "cloudflare 500"},
		{name: "terminated is final",
			status: map[string]interface{}{"phase": "Terminated", "message": "session ended"},
			is:     ErrTerminated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(false)
			ex, err := Expose(context.Background(), c, validRequest())
			if err != nil {
				t.Fatal(err)
			}
			setStatus(t, c, ex, tc.status)
			ep, err := Wait(context.Background(), c, ex, opts)
			switch {
			case tc.wantURL != "":
				if err != nil || ep.URL != tc.wantURL || ep.Hostname != "s1.example.com" {
					t.Fatalf("ep=%+v err=%v", ep, err)
				}
			case tc.is != nil:
				if !errors.Is(err, tc.is) {
					t.Fatalf("err = %v", err)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			}
		})
	}
}

// TestWaitToleratesMissingTunnelThenSucceeds checks the behaviour its name describes.
func TestWaitToleratesMissingTunnelThenSucceeds(t *testing.T) {
	c := newClient(false)
	ex := &Exposure{Namespace: "agents", SessionName: "s1", TunnelName: "s1"}
	go func() {
		time.Sleep(20 * time.Millisecond)
		tunnel := newObject(TunnelGVK, "agents", "s1")
		tunnel.Object["status"] = map[string]interface{}{"phase": "Active", "connectorReady": true, "hostname": "h.example.com"}
		_ = c.Create(context.Background(), tunnel)
	}()
	ep, err := Wait(context.Background(), c, ex, WaitOptions{Interval: 2 * time.Millisecond, Timeout: 2 * time.Second})
	if err != nil || ep.Hostname != "h.example.com" {
		t.Fatalf("ep=%+v err=%v", ep, err)
	}
}

// TestWaitStopsOnContextCancel checks the behaviour its name describes.
func TestWaitStopsOnContextCancel(t *testing.T) {
	c := newClient(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Wait(ctx, c, &Exposure{Namespace: "agents", TunnelName: "nope"}, WaitOptions{Interval: time.Millisecond})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestNilExposure checks the behaviour its name describes.
func TestNilExposure(t *testing.T) {
	c := newClient(false)
	if err := Complete(context.Background(), c, nil); err == nil {
		t.Fatal("Complete: expected error")
	}
	if _, err := Wait(context.Background(), c, nil, WaitOptions{}); err == nil {
		t.Fatal("Wait: expected error")
	}
}

// TestExposeCleansUpEvenWhenContextIsCancelled checks the behaviour its name describes.
func TestExposeCleansUpEvenWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(cctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID("uid")
			if obj.GetObjectKind().GroupVersionKind().Kind == TunnelGVK.Kind {
				cancel()
				return context.Canceled
			}
			return cl.Create(cctx, obj, opts...)
		},
		Delete: func(dctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := dctx.Err(); err != nil {
				return err
			}
			return cl.Delete(dctx, obj, opts...)
		},
	})
	if _, err := Expose(ctx, c, validRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	session := newObject(SessionGVK, "agents", "s1")
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(session), session); !apierrors.IsNotFound(err) {
		t.Fatalf("session should be gone, got %v", err)
	}
}

// TestExposeCleanupIsPinnedToTheCreatedSessionUID checks the behaviour its name describes.
func TestExposeCleanupIsPinnedToTheCreatedSessionUID(t *testing.T) {
	var got *client.DeleteOptions
	base := newClient(true)
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			got = &client.DeleteOptions{}
			got.ApplyOptions(opts)
			return cl.Delete(ctx, obj, opts...)
		},
	})
	if _, err := Expose(context.Background(), c, validRequest()); err == nil {
		t.Fatal("expected an error")
	}
	if got == nil || got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != "uid-s1" {
		t.Fatalf("cleanup delete was not pinned to the session UID: %+v", got)
	}
}

// TestCompleteRefusesAReplacedSession checks that a session recreated under the same name is left alone.
func TestCompleteRefusesAReplacedSession(t *testing.T) {
	c := newClient(false)
	ex, err := Expose(context.Background(), c, validRequest())
	if err != nil {
		t.Fatal(err)
	}
	old := newObject(SessionGVK, "agents", "s1")
	get(t, c, SessionGVK, old)
	if err := c.Delete(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	repl := newObject(SessionGVK, "agents", "s1")
	repl.SetUID("replacement")
	repl.Object["spec"] = map[string]interface{}{"poolRef": map[string]interface{}{"name": "pool"}}
	if err := c.Create(context.Background(), repl); err != nil {
		t.Fatal(err)
	}

	if err := Complete(context.Background(), c, ex); err == nil || !strings.Contains(err.Error(), "metadata/uid") {
		t.Fatalf("expected the UID test to fail, got %v", err)
	}
	got := newObject(SessionGVK, "agents", "s1")
	get(t, c, SessionGVK, got)
	if done, _, _ := unstructured.NestedBool(got.Object, "spec", "completed"); done {
		t.Fatal("the replacement session was completed")
	}
}

// TestWaitRefusesAReplacedTunnel checks that Wait does not return another exposure's endpoint.
func TestWaitRefusesAReplacedTunnel(t *testing.T) {
	c := newClient(false)
	ex, err := Expose(context.Background(), c, validRequest())
	if err != nil {
		t.Fatal(err)
	}
	old := newObject(TunnelGVK, "agents", "s1")
	get(t, c, TunnelGVK, old)
	if err := c.Delete(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	repl := newObject(TunnelGVK, "agents", "s1")
	repl.SetUID("other")
	repl.Object["status"] = map[string]interface{}{"phase": "Active", "connectorReady": true, "hostname": "evil.example.com"}
	if err := c.Create(context.Background(), repl); err != nil {
		t.Fatal(err)
	}
	ep, err := Wait(context.Background(), c, ex, WaitOptions{Interval: time.Millisecond, Timeout: 50 * time.Millisecond})
	if !errors.Is(err, ErrTerminated) || ep != nil {
		t.Fatalf("ep=%+v err=%v", ep, err)
	}
}

// TestCompleteRequiresSessionUID checks that Complete refuses an exposure without a session UID.
func TestCompleteRequiresSessionUID(t *testing.T) {
	c := newClient(false)
	session := newObject(SessionGVK, "agents", "s1")
	session.Object["spec"] = map[string]interface{}{}
	if err := c.Create(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	err := Complete(context.Background(), c, &Exposure{Namespace: "agents", SessionName: "s1"})
	if err == nil || !strings.Contains(err.Error(), "no SessionUID") {
		t.Fatalf("expected missing SessionUID error, got %v", err)
	}
	got := newObject(SessionGVK, "agents", "s1")
	get(t, c, SessionGVK, got)
	if done, _, _ := unstructured.NestedBool(got.Object, "spec", "completed"); done {
		t.Fatal("session was completed")
	}
}
