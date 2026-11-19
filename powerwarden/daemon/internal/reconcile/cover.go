package reconcile

// Covers reports whether have is a superset of need.
// An empty need is covered by every have.
func Covers(have, need []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, t := range have {
		set[t] = struct{}{}
	}
	for _, t := range need {
		if _, ok := set[t]; !ok {
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool { return Covers(a, b) && Covers(b, a) }
