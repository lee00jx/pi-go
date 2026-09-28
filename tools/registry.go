// Package tools hosts concrete tool implementations and the registry.
//
// Built-ins: bash, read, write, edit (1:1 with pi, no sandbox). sql is
// deliberately not implemented (pi has none). recall_event lives in core
// (SessionTool, compaction escape hatch). Business tools are registered
// by integrators through the same core.Tool interface.
package tools

import "github.com/lee00jx/pi-go/core"

// Registry is the tool table handed to core.Runner.
type Registry struct {
	order []string
	tools map[string]core.Tool
}

// NewRegistry builds a registry pre-loaded with the given tools.
func NewRegistry(tools ...core.Tool) *Registry {
	r := &Registry{tools: make(map[string]core.Tool, len(tools))}
	for _, t := range tools {
		r.Register(t)
	}
	return r
}

// Register adds (or replaces) a tool.
func (r *Registry) Register(t core.Tool) {
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// Get looks a tool up by name.
func (r *Registry) Get(name string) (core.Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List returns the tools in registration order.
func (r *Registry) List() []core.Tool {
	out := make([]core.Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}
