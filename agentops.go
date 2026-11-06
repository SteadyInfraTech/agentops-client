// Package agentops creates an AgentSession and the AgentEdgeTunnel that
// exposes it, and waits for the public endpoint.
//
// Both kinds are handled as unstructured, so this package depends on neither
// operator. The fields it reads and writes are the contract described in
// agentops-operator's docs/design/edge-exposure.md.
package agentops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The kinds this package works with.
var (
	// SessionGVK is agentops-operator's AgentSession.
	SessionGVK = schema.GroupVersionKind{Group: "agents.steadyinfra.com", Version: "v1alpha1", Kind: "AgentSession"}
	// TunnelGVK is ai-agent-edge-tunnel's AgentEdgeTunnel.
	TunnelGVK = schema.GroupVersionKind{Group: "edge.steadyinfra.com", Version: "v1alpha1", Kind: "AgentEdgeTunnel"}
)

// cleanupTimeout bounds the delete that undoes a half-finished Expose.
const cleanupTimeout = 10 * time.Second

// ExposeRequest describes a session and how to reach it from outside.
type ExposeRequest struct {
	// Namespace and Name for both objects. The session and the tunnel share the
	// name; they are different kinds.
	Namespace string
	Name      string

	// Exactly one of PoolName and AgentServiceName selects the agent.
	PoolName         string
	AgentServiceName string
	// UserID and Parameters are passed to the session as they are.
	UserID     string
	Parameters map[string]string
	// MaxDurationSeconds is the session's lifetime after binding. Nil keeps the
	// operator's default; 0 means no limit.
	MaxDurationSeconds *int64

	// ZoneID and Subdomain place the hostname under the edge operator's base
	// domain. Subdomain must be a single DNS label.
	ZoneID    string
	Subdomain string
	// Exactly one of AllowedEmails and AllowPublic. A tunnel is never public by
	// omission.
	AllowedEmails []string
	AllowPublic   bool
	// TTL is an optional hard cap on the tunnel. The session's phase ends the
	// tunnel first, so leave it zero unless bounded exposure matters more than
	// session continuity.
	TTL time.Duration
}

// Exposure identifies the objects Expose created.
type Exposure struct {
	Namespace   string
	SessionName string
	TunnelName  string
	// SessionUID is the UID of the created session. Complete only acts on a
	// session with this UID.
	SessionUID types.UID
	// TunnelUID is the UID of the created tunnel. When set, Wait only trusts a
	// tunnel with this UID.
	TunnelUID types.UID
}

// Validate reports a request that could never produce a working exposure.
func (r ExposeRequest) Validate() error {
	if r.Namespace == "" || r.Name == "" {
		return errors.New("namespace and name are required")
	}
	if errs := validation.IsDNS1123Subdomain(r.Name); len(errs) > 0 {
		return fmt.Errorf("invalid name %q: %v", r.Name, errs)
	}
	if (r.PoolName == "") == (r.AgentServiceName == "") {
		return errors.New("set exactly one of PoolName and AgentServiceName")
	}
	if r.ZoneID == "" || r.Subdomain == "" {
		return errors.New("ZoneID and Subdomain are required")
	}
	if errs := validation.IsDNS1123Label(r.Subdomain); len(errs) > 0 {
		return fmt.Errorf("Subdomain must be a single DNS label: %v", errs)
	}
	if (len(r.AllowedEmails) > 0) == r.AllowPublic {
		return errors.New("set exactly one of AllowedEmails and AllowPublic")
	}
	if r.TTL < 0 {
		return errors.New("TTL must not be negative")
	}
	if r.MaxDurationSeconds != nil && *r.MaxDurationSeconds < 0 {
		return errors.New("MaxDurationSeconds must not be negative")
	}
	return nil
}

// Expose creates the AgentSession, then the AgentEdgeTunnel that follows it.
// The tunnel carries an ownerReference to the session, so deleting the session
// deletes the tunnel, and the edge operator also removes the tunnel's
// Cloudflare resources when the session ends. If the tunnel cannot be created,
// the session is deleted again so a failed call leaves nothing behind.
//
// The tunnel is not usable yet when Expose returns: call Wait.
func Expose(ctx context.Context, c client.Client, req ExposeRequest) (*Exposure, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	session := newObject(SessionGVK, req.Namespace, req.Name)
	session.Object["spec"] = sessionSpec(req)
	if err := c.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("creating AgentSession: %w", err)
	}
	if session.GetUID() == "" {
		return nil, errors.New("the API server returned the AgentSession without a UID")
	}

	tunnel := newObject(TunnelGVK, req.Namespace, req.Name)
	tunnel.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: SessionGVK.GroupVersion().String(),
		Kind:       SessionGVK.Kind,
		Name:       session.GetName(),
		UID:        session.GetUID(),
	}})
	tunnel.Object["spec"] = tunnelSpec(req)
	if err := c.Create(ctx, tunnel); err != nil {
		// ctx may be what made the create fail, so clean up on a context that
		// survives its cancellation.
		// Only delete the session this call created: if it was replaced in the
		// meantime, the replacement is not ours to remove.
		uid := session.GetUID()
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		cleanupErr := client.IgnoreNotFound(c.Delete(cleanCtx, session, client.Preconditions{UID: &uid}))
		return nil, errors.Join(fmt.Errorf("creating AgentEdgeTunnel: %w", err), cleanupErr)
	}

	return &Exposure{
		Namespace:   req.Namespace,
		SessionName: session.GetName(),
		TunnelName:  tunnel.GetName(),
		SessionUID:  session.GetUID(),
		TunnelUID:   tunnel.GetUID(),
	}, nil
}

// Complete marks the session finished, if it is still the one Expose created. The session releases its capacity and
// the edge operator then removes the tunnel.
func Complete(ctx context.Context, c client.Client, e *Exposure) error {
	if e == nil {
		return errors.New("exposure is nil")
	}
	if e.SessionUID == "" {
		return errors.New("exposure has no SessionUID; use the value returned by Expose")
	}
	session := newObject(SessionGVK, e.Namespace, e.SessionName)
	// Test the UID in the same atomic patch, so a session deleted and recreated
	// under this name is not completed by mistake. The API server rejects the
	// patch if the UID differs.
	ops, err := json.Marshal([]map[string]interface{}{
		{"op": "test", "path": "/metadata/uid", "value": string(e.SessionUID)},
		{"op": "add", "path": "/spec/completed", "value": true},
	})
	if err != nil {
		return err
	}
	return c.Patch(ctx, session, client.RawPatch(types.JSONPatchType, ops))
}

// Endpoint is where a client reaches the exposed agent.
type Endpoint struct {
	Hostname string
	// URL is https://<hostname>.
	URL string
}

// WaitOptions tunes Wait.
type WaitOptions struct {
	// Interval between checks. Default 2s.
	Interval time.Duration
	// Timeout bounds the wait. Zero waits until ctx is done.
	Timeout time.Duration
}

// ErrTerminated means the tunnel ended before it became usable.
var ErrTerminated = errors.New("the AgentEdgeTunnel was terminated")

// Wait returns once the tunnel is Active and its connector is connected. A
// hostname in status does not mean the endpoint works: it appears while the
// tunnel is still Pending. Failed is not final, because the edge operator
// retries transient errors, so Wait keeps watching through it and reports the
// last message if time runs out. Only Terminated ends the wait early.
func Wait(ctx context.Context, c client.Client, e *Exposure, opts WaitOptions) (*Endpoint, error) {
	if e == nil {
		return nil, errors.New("exposure is nil")
	}
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	last := "no status yet"
	for {
		tunnel := newObject(TunnelGVK, e.Namespace, e.TunnelName)
		err := c.Get(ctx, client.ObjectKeyFromObject(tunnel), tunnel)
		switch {
		case err == nil:
			if e.TunnelUID != "" && tunnel.GetUID() != e.TunnelUID {
				return nil, fmt.Errorf("%w: the AgentEdgeTunnel %q was replaced by another object", ErrTerminated, e.TunnelName)
			}
			phase, _, _ := unstructured.NestedString(tunnel.Object, "status", "phase")
			message, _, _ := unstructured.NestedString(tunnel.Object, "status", "message")
			hostname, _, _ := unstructured.NestedString(tunnel.Object, "status", "hostname")
			ready, _, _ := unstructured.NestedBool(tunnel.Object, "status", "connectorReady")
			last = fmt.Sprintf("phase %q, connector ready %t: %s", phase, ready, message)
			switch {
			case phase == "Terminated":
				return nil, fmt.Errorf("%w: %s", ErrTerminated, message)
			case phase == "Active" && ready && hostname != "":
				return &Endpoint{Hostname: hostname, URL: "https://" + hostname}, nil
			}
		case apierrors.IsNotFound(err):
			last = "the AgentEdgeTunnel does not exist yet"
		case ctx.Err() == nil:
			return nil, fmt.Errorf("reading the AgentEdgeTunnel: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("gave up waiting (%v); last status: %s", ctx.Err(), last)
		case <-time.After(opts.Interval):
		}
	}
}

// newObject returns an empty unstructured object of kind gvk.
func newObject(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

// sessionSpec renders the AgentSession spec for req.
func sessionSpec(req ExposeRequest) map[string]interface{} {
	spec := map[string]interface{}{}
	if req.PoolName != "" {
		spec["poolRef"] = map[string]interface{}{"name": req.PoolName}
	} else {
		spec["agentServiceRef"] = map[string]interface{}{"name": req.AgentServiceName}
	}
	if req.UserID != "" {
		spec["userID"] = req.UserID
	}
	if len(req.Parameters) > 0 {
		params := make(map[string]interface{}, len(req.Parameters))
		for k, v := range req.Parameters {
			params[k] = v
		}
		spec["parameters"] = params
	}
	if req.MaxDurationSeconds != nil {
		spec["maxDurationSeconds"] = *req.MaxDurationSeconds
	}
	return spec
}

// tunnelSpec renders the AgentEdgeTunnel spec for req.
func tunnelSpec(req ExposeRequest) map[string]interface{} {
	security := map[string]interface{}{}
	if req.AllowPublic {
		security["allowPublic"] = true
	} else {
		emails := make([]interface{}, len(req.AllowedEmails))
		for i, e := range req.AllowedEmails {
			emails[i] = e
		}
		security["allowedEmails"] = emails
	}
	spec := map[string]interface{}{
		"sessionRef": map[string]interface{}{"name": req.Name},
		"cloudflare": map[string]interface{}{"zoneID": req.ZoneID, "subdomain": req.Subdomain},
		"security":   security,
	}
	if req.TTL > 0 {
		spec["ttl"] = req.TTL.String()
	}
	return spec
}
