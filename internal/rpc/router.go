// Package rpc maps method names to handlers. It is transport agnostic: the
// tunnel client and the local HTTP API both dispatch through a Router.
package rpc

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/ababank/mobile-device-agent/internal/protocol"
)

// Handler executes one method call.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Router is a concurrency-safe method table.
type Router struct {
	mu sync.RWMutex
	h  map[string]Handler
}

func NewRouter() *Router { return &Router{h: map[string]Handler{}} }

func (r *Router) Handle(method string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.h[method] = h
}

// Methods returns the registered method names, sorted.
func (r *Router) Methods() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.h))
	for m := range r.h {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Call dispatches a method.
func (r *Router) Call(ctx context.Context, method string, params json.RawMessage) (any, error) {
	r.mu.RLock()
	h, ok := r.h[method]
	r.mu.RUnlock()
	if !ok {
		return nil, protocol.Errorf(protocol.CodeNotFound, "unknown method %q", method)
	}
	return h(ctx, params)
}

// Typed adapts a handler taking a decoded params struct.
func Typed[P any](fn func(ctx context.Context, p P) (any, error)) Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p P
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, protocol.Errorf(protocol.CodeBadRequest, "invalid params: %v", err)
			}
		}
		return fn(ctx, p)
	}
}
