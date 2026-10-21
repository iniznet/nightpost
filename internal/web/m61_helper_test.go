package web

import "github.com/iniznet/nightpost/internal/store"

// store_ProfileInputFront is a named constructor so the route tests can seed a
// traced, tree-bearing profile without importing the store package twice.
func store_ProfileInputFront() store.ProfileInput {
	return store.ProfileInput{
		ProjectKey: "front",
		Type:       store.ProfileTypeRequest,
		Name:       "GET /chapters",
		DurationMs: 300,
		TraceID:    "t1",
		Tree:       []store.TreeNode{{N: "main()", S: 20, I: 300}},
	}
}
