package ingest

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

// Call-tree caps : a production PHP request explodes into thousands of
// nodes, so the stored tree is pruned to what a human can scan: subtrees
// under max(1% of total, 2 ms) fold into their parent, wide siblings
// collapse into an "(other xN)" node, depth is capped, and the marshaled
// JSON must stay under the byte ceiling or the tree is dropped entirely
// (the flat hotspot list survives — telemetry never loses the row).
const (
	treeMaxDepth    = 12
	treeMaxChildren = 20
	treeMaxBytes    = 64 << 10
	treeMinAbsMs    = 2.0
)

type treeChild struct {
	name  string
	ms    float64
	calls int64
}

// BuildCallTree reconstructs a pruned stack tree from the xhprof edge map
// ("parent==>child", inclusive wall-clock microseconds per call site).
// Rooted at main(); functions with multiple parents explode per call site,
// which is exactly the "how did it execute" shape the trace page renders.
func BuildCallTree(edges map[string]xhprofEntry) []store.TreeNode {
	root := edges["main()"]
	if root.Wt <= 0 {
		return nil
	}
	totalMs := root.Wt / 1000.0
	children := map[string][]treeChild{}
	for key, e := range edges {
		parent, child, ok := strings.Cut(key, "==>")
		if !ok || e.Wt <= 0 {
			continue
		}
		children[parent] = append(children[parent], treeChild{name: child, ms: e.Wt / 1000.0, calls: e.Ct})
	}
	node := expandTree("main()", totalMs, 1, 0, totalMs, children, map[string]bool{})
	nodes := []store.TreeNode{node}
	if !treeFitsBytes(nodes) {
		return nil
	}
	return nodes
}

func expandTree(name string, inclMs float64, calls int64, depth int, totalMs float64, children map[string][]treeChild, path map[string]bool) store.TreeNode {
	node := store.TreeNode{N: name, I: roundMs(inclMs), C: calls}
	if depth >= treeMaxDepth || path[name] {
		// Depth cap or recursion cycle: everything below is this node's self.
		node.S = node.I
		return node
	}
	kids := children[name]
	if len(kids) == 0 {
		node.S = node.I
		return node
	}
	sorted := make([]treeChild, len(kids))
	copy(sorted, kids)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ms > sorted[j].ms })
	floor := totalMs / 100.0
	if floor < treeMinAbsMs {
		floor = treeMinAbsMs
	}
	var expanded []store.TreeNode
	var expandedSum float64
	var widthSum float64
	var widthCalls int64
	var widthCount int
	for i, k := range sorted {
		switch {
		case i >= treeMaxChildren:
			// Wide tails stay visible as a single honest bucket.
			widthSum += k.ms
			widthCalls += k.calls
			widthCount++
		case k.ms < floor || path[k.name]:
			// Below the visibility floor (or a recursion cycle): the time is
			// real but unexpandable — it lands silently in this node's self.
		default:
			path[name] = true
			expanded = append(expanded, expandTree(k.name, k.ms, k.calls, depth+1, totalMs, children, path))
			delete(path, name)
			expandedSum += k.ms
		}
	}
	if widthCount > 0 {
		expanded = append(expanded, store.TreeNode{N: "(other ×" + strconv.Itoa(widthCount) + ")", I: roundMs(widthSum), S: roundMs(widthSum), C: widthCalls})
	}
	self := inclMs - expandedSum - widthSum
	if self < 0 {
		self = 0
	}
	node.S = roundMs(self)
	node.K = expanded
	return node
}

// SanitizeTree re-enforces the caps on a CLIENT-SENT tree (the SvelteKit
// front prunes locally but the server is the trust boundary): depth, width,
// negative times, and the byte ceiling. An oversized tree is dropped.
func SanitizeTree(nodes []store.TreeNode, totalMs float64) []store.TreeNode {
	out := sanitizeLevel(nodes, 0, totalMs)
	if len(out) == 0 || !treeFitsBytes(out) {
		return nil
	}
	return out
}

func sanitizeLevel(nodes []store.TreeNode, depth int, totalMs float64) []store.TreeNode {
	if depth >= treeMaxDepth || len(nodes) == 0 {
		return nil
	}
	sorted := make([]store.TreeNode, len(nodes))
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].I > sorted[j].I })
	floor := totalMs / 100.0
	if floor < treeMinAbsMs {
		floor = treeMinAbsMs
	}
	var keep []store.TreeNode
	var widthSum float64
	var widthC int64
	var widthN int
	for i, n := range sorted {
		switch {
		case i >= treeMaxChildren:
			widthSum += n.I
			widthC += n.C
			widthN++
		case n.I < floor:
			// Pruned subtrees fold into the parent's self below.
		default:
			clean := store.TreeNode{N: strings.Clone(n.N), I: roundMs(n.I), S: roundMs(n.S), C: n.C}
			if clean.I < 0 {
				clean.I = 0
			}
			if clean.S < 0 {
				clean.S = 0
			}
			clean.S += foldedIntoSelf(n, floor)
			clean.K = sanitizeLevel(n.K, depth+1, totalMs)
			keep = append(keep, clean)
		}
	}
	if widthN > 0 {
		keep = append(keep, store.TreeNode{N: "(other ×" + strconv.Itoa(widthN) + ")", I: roundMs(widthSum), S: roundMs(widthSum), C: widthC})
	}
	return keep
}

// foldedIntoSelf sums the below-floor children whose time silently becomes
// the parent's self after pruning, using the same tree-wide floor.
func foldedIntoSelf(n store.TreeNode, floor float64) float64 {
	var sum float64
	for _, k := range n.K {
		if k.I < floor {
			sum += k.I
		}
	}
	return roundMs(sum)
}

func treeFitsBytes(nodes []store.TreeNode) bool {
	b, err := json.Marshal(nodes)
	if err != nil {
		return false
	}
	return len(b) <= treeMaxBytes
}

func roundMs(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
