// Package diff lines two manifests up side by side, so the race screen can show
// them with the same line at the same height and only the differences marked.
package diff

import "strings"

// Row is one line of the side-by-side view. Left or Right is empty where only
// the other manifest has a line, which keeps the lines below aligned.
type Row struct {
	Left  string `json:"left"`
	Right string `json:"right"`
	Kind  string `json:"kind"` // same, changed, left (only on the left), right
}

// Lines aligns a and b on their longest common subsequence. Within a run of
// lines that differ, a line pairs with the other side's line for the same field
// or flag, so a changed value sits next to what it replaced.
func Lines(a, b []string) []Row {
	// lcs[i][j] is the LCS length of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var rows []Row
	var onlyA, onlyB []string
	// flush pairs a run of differing lines as changed only where they are the
	// same field or flag, so --kv-cache-dtype never shows as having become
	// --speculative-config. Everything else gets a row to itself.
	flush := func() {
		used := make([]bool, len(onlyB))
		next := 0 // lines of onlyB before next are already placed
		for _, l := range onlyA {
			match := -1
			for k := next; k < len(onlyB); k++ {
				if !used[k] && key(onlyB[k]) == key(l) {
					match = k
					break
				}
			}
			if match < 0 {
				rows = append(rows, Row{Left: l, Kind: "left"})
				continue
			}
			for ; next < match; next++ {
				if !used[next] {
					rows = append(rows, Row{Right: onlyB[next], Kind: "right"})
					used[next] = true
				}
			}
			rows = append(rows, Row{Left: l, Right: onlyB[match], Kind: "changed"})
			used[match] = true
			next = match + 1
		}
		for k, r := range onlyB {
			if !used[k] {
				rows = append(rows, Row{Right: r, Kind: "right"})
			}
		}
		onlyA, onlyB = nil, nil
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			flush()
			rows = append(rows, Row{Left: a[i], Right: b[j], Kind: "same"})
			i, j = i+1, j+1
		case j == len(b) || (i < len(a) && lcs[i+1][j] >= lcs[i][j+1]):
			onlyA = append(onlyA, a[i])
			i++
		default:
			onlyB = append(onlyB, b[j])
			j++
		}
	}
	flush()
	return rows
}

// key is what a line sets, without the value: "name" for "name: x", and
// "--model" for "- --model=x". Lines with the same key are versions of each
// other.
func key(line string) string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "- ")
	t = strings.Trim(t, "'\"")
	if i := strings.IndexAny(t, "=:"); i >= 0 {
		t = t[:i]
	}
	return strings.Repeat(" ", len(line)-len(strings.TrimLeft(line, " "))) + t
}
