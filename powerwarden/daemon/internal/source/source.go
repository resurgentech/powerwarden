// Package source provides demand, liveness and claims to the reconciler: the
// rows hourchd, agents and operators write through the API.
package source

import (
	"context"
	"time"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

// Snapshot is one load of demand, liveness and claims.
type Snapshot struct {
	// Demands are open demands (lease_until > now).
	Demands []model.Demand
	// Workers are all worker rows, including expired ones (busy-hold records).
	Workers []model.Worker
	// Claims are live claims (not lapsed: expires_at and, unless detached,
	// lease_until in the future), failed ones included, oldest first.
	Claims []model.Claim
}

// Source is a demand and liveness provider.
type Source interface {
	Load(ctx context.Context, now time.Time) (Snapshot, error)
}

// HTTP is the source: rows written through the API.
type HTTP struct{ Store *store.Store }

func (h HTTP) Load(ctx context.Context, now time.Time) (Snapshot, error) {
	d, err := h.Store.ListDemands(ctx, now, false)
	if err != nil {
		return Snapshot{}, err
	}
	w, err := h.Store.ListWorkers(ctx, now, true)
	if err != nil {
		return Snapshot{}, err
	}
	c, err := h.Store.ListClaims(ctx, now, store.ClaimFilter{})
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Demands: d, Workers: w, Claims: c}, nil
}
