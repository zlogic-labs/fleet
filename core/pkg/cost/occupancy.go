package cost

import "sort"

// occupied sweeps every deployment, returning what each one used and who had it.
//
// Both figures come from one sweep so they cannot disagree: a deployment's used
// time is by definition the sum of the key shares taken out of it.
func occupied(in Input) (map[string]int64, []Use) {
	names := deploymentNames(in)
	used := make(map[string]int64, len(names))
	totals := make(map[string]int64, len(names))

	for _, name := range names {
		b := Sweep(in.Reserved[name], in.Spans[name], in.Period.Start, in.Period.End)
		used[name] = b.Seconds
		for key, v := range b.ByKey {
			totals[key] += v
		}
	}

	uses := make([]Use, 0, len(totals))
	for key, v := range totals {
		if v > 0 {
			uses = append(uses, Use{Key: key, GPUSeconds: v})
		}
	}
	sort.Slice(uses, func(i, j int) bool { return uses[i].Key < uses[j].Key })
	return used, uses
}

// usedSeconds sums what the sweep found, which is the busy time the report
// prices. Not the sum of every key's share, which is the same number but
// arrives by a longer route that a future change could quietly break.
func usedSeconds(used map[string]int64) int64 {
	var sum int64
	for _, v := range used {
		sum += v
	}
	return sum
}
