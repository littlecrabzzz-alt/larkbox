package middleware

import (
	"context"
	"net/http"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type resourceOwnerKey struct{}

// ResourceOwner returns the identity admitted by OwnedResource.
func ResourceOwner(r *http.Request) store.ResourceOwner {
	owner, _ := r.Context().Value(resourceOwnerKey{}).(store.ResourceOwner)
	return owner
}

// OwnedResource authenticates requests and binds an owner for tenant-scoped APIs.
func OwnedResource(state *store.Store, required RequiredToken, strict bool, action http.HandlerFunc, scopes ...string) http.Handler {
	return Bearer(state, required, strict)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, authenticated := AuthFromContext(r.Context())
		var owner store.ResourceOwner
		if authenticated {
			if strict && (identity.Kind == store.UserToken || identity.Scope != "") && !RequireOneScope(w, identity, scopes...) {
				return
			}
			var valid bool
			owner, valid = state.ResourceOwner(identity.AppID, identity.UserOpenID)
			if !valid || owner.TenantKey != identity.TenantKey {
				_ = protocol.WriteError(w, credentialError(identity.Kind, invalidCredential))
				return
			}
		} else if apps := state.Apps().List(); len(apps) > 0 {
			owner = store.ResourceOwner{TenantKey: apps[0].TenantKey, AppID: apps[0].AppID}
		}
		action(w, r.WithContext(context.WithValue(r.Context(), resourceOwnerKey{}, owner)))
	}))
}
