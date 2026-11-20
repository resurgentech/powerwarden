package httpapi

import (
	"time"
	"unicode"

	"github.com/resurgentech/keyclient/keylist"
	api "github.com/resurgentech/powerwarden/powermaster/api"
)

// maxLastErrorRunes caps the last_error text of GET /v1/stats.
const maxLastErrorRunes = 200

// personalKeysStatus maps keyclient's Status to the stats object. Values go
// straight through (times are not rounded or moved to another zone); age and
// list_time are present only when a list is live, last_success only when
// there has been a good pull, last_error only after a failed attempt, with
// every non-printable rune replaced by '?' and cut to 200 runes.
func personalKeysStatus(st keylist.Status) api.PersonalKeysStatus {
	out := api.PersonalKeysStatus{
		Origin:       api.PersonalKeysStatusOrigin(st.Origin),
		Entries:      st.Entries,
		CacheFailing: st.CacheFailing,
	}
	if st.Origin != keylist.OriginNone {
		age := int(st.Age / time.Second)
		lt := st.ListTime.UTC()
		out.AgeSeconds, out.ListTime = &age, &lt
	}
	if !st.LastSuccess.IsZero() {
		ls := st.LastSuccess.UTC()
		out.LastSuccess = &ls
	}
	if st.LastError != nil {
		rs := make([]rune, 0, maxLastErrorRunes)
		for _, r := range st.LastError.Error() {
			if len(rs) == maxLastErrorRunes {
				break
			}
			if !unicode.IsPrint(r) {
				r = '?'
			}
			rs = append(rs, r)
		}
		msg := string(rs)
		out.LastError = &msg
	}
	return out
}
