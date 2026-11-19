package store

import (
	"fmt"
	"regexp"
	"slices"
)

var (
	DemandIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,160}$`)
	WorkerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	TagPattern      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,253}$`)
)

const maxExternalRef = 512

func badf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, a...))
}

// normTags validates tags and returns them sorted and de-duplicated.
func normTags(tags []string, max int) ([]string, error) {
	if len(tags) > max {
		return nil, badf("at most %d tags", max)
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !TagPattern.MatchString(t) {
			return nil, badf("tag %q does not match %s", t, TagPattern)
		}
		out = append(out, t)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
