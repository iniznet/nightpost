package ingest

import (
	"strings"
	"testing"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

func TestBuildCallTreePrunesBelowFloor(t *testing.T) {
	// main 1000ms; Slow 600 (Deep 400 inside); Tiny 5ms < floor(10ms)
	edges := map[string]xhprofEntry{
		"main()":                               {Ct: 1, Wt: 1_000_000},
		"main()==>App\\Slow::handle":           {Ct: 1, Wt: 600_000},
		"App\\Slow::handle==>App\\Deep::query": {Ct: 1, Wt: 400_000},
		"main()==>App\\Tiny::noise":            {Ct: 7, Wt: 5_000},
	}
	tree := BuildCallTree(edges)
	if len(tree) != 1 || tree[0].N != "main()" {
		t.Fatalf("root = %+v", tree)
	}
	root := tree[0]
	if root.I != 1000 || root.S != 400 {
		t.Errorf("root I/S = %v/%v, want 1000/400 (tiny folded into self)", root.I, root.S)
	}
	if len(root.K) != 1 || root.K[0].N != "App\\Slow::handle" {
		t.Fatalf("children = %+v", root.K)
	}
	slow := root.K[0]
	if slow.I != 600 || slow.S != 200 {
		t.Errorf("slow I/S = %v/%v, want 600/200", slow.I, slow.S)
	}
	if len(slow.K) != 1 || slow.K[0].I != 400 {
		t.Errorf("deep child missing: %+v", slow.K)
	}
	if root.S+slow.I != root.I {
		t.Errorf("root invariant broken: %v + %v != %v", root.S, slow.I, root.I)
	}
}

func TestBuildCallTreeCollapsesWideTails(t *testing.T) {
	edges := map[string]xhprofEntry{"main()": {Ct: 1, Wt: 1_000_000}}
	for i := 0; i < 25; i++ {
		name := "fn" + string(rune(97+i))
		edges["main()==>"+name] = xhprofEntry{Ct: 1, Wt: 30_000}
	}
	tree := BuildCallTree(edges)
	if len(tree) != 1 || len(tree[0].K) != treeMaxChildren+1 {
		t.Fatalf("want %d children + 1 bucket, got %d", treeMaxChildren, len(tree[0].K))
	}
	bucket := tree[0].K[treeMaxChildren]
	if !strings.HasPrefix(bucket.N, "(other ") {
		t.Errorf("last child = %q, want (other xN) bucket", bucket.N)
	}
	if bucket.I != 150 {
		t.Errorf("bucket incl = %v, want 150 (5 x 30ms)", bucket.I)
	}
	// Invariant: self + sum(children incl) == incl.
	if tree[0].S+600+150 != tree[0].I {
		t.Errorf("root self %v inconsistent with incl %v", tree[0].S, tree[0].I)
	}
}

func TestBuildCallTreeSurvivesRecursionCycles(t *testing.T) {
	edges := map[string]xhprofEntry{
		"main()":     {Ct: 1, Wt: 1_000_000},
		"main()==>A": {Ct: 1, Wt: 800_000},
		"A==>B":      {Ct: 1, Wt: 700_000},
		"B==>A":      {Ct: 1, Wt: 600_000},
	}
	tree := BuildCallTree(edges)
	if len(tree) != 1 || len(tree[0].K) != 1 {
		t.Fatalf("tree = %+v", tree)
	}
	a := tree[0].K[0]
	if len(a.K) != 1 {
		t.Fatalf("A children = %+v", a.K)
	}
	b := a.K[0]
	if b.S != 700 || len(b.K) != 0 {
		t.Errorf("cycle not folded: b = %+v", b)
	}
}

func TestSanitizeTreeEnforcesCaps(t *testing.T) {
	nodes := []store.TreeNode{{N: "root", I: 1000, S: -5, K: []store.TreeNode{
		{N: "keep", I: 900, S: 900},
		{N: "floor-pruned", I: 1, S: 1},
	}}}
	out := SanitizeTree(nodes, 1000)
	if len(out) != 1 {
		t.Fatalf("roots = %+v", out)
	}
	// Negative self clamps to 0, then the below-floor child folds its 1ms in.
	if out[0].S != 1 || len(out[0].K) != 1 || out[0].K[0].N != "keep" {
		t.Fatalf("sanitize result = %+v", out[0])
	}
	// Deep chain trims at the cap.
	deep := store.TreeNode{N: "r", I: 1000, S: 0}
	cur := &deep
	for i := 0; i < 30; i++ {
		child := store.TreeNode{N: "c", I: 900, S: 0}
		cur.K = []store.TreeNode{child}
		cur = &cur.K[0]
	}
	trimmed := SanitizeTree([]store.TreeNode{deep}, 1000)
	d := 0
	for len(trimmed) > 0 {
		n := trimmed[0]
		d++
		if d > treeMaxDepth {
			t.Fatalf("depth cap not enforced: %d levels", d)
		}
		trimmed = n.K
	}
}

func TestBuildCallTreeEmptyWithoutRoot(t *testing.T) {
	if tree := BuildCallTree(map[string]xhprofEntry{"nope": {Wt: 5}}); tree != nil {
		t.Errorf("want nil for rootless edges, got %+v", tree)
	}
}
