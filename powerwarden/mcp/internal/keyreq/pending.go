package keyreq

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"time"

	"github.com/resurgentech/keyclient/request"
)

// PendingInfo is what `key status` shows of a pending request: nothing that
// would let anyone poll it.
type PendingInfo struct {
	Service string
	Code    string
	Expires time.Time
}

var reUserCode = regexp.MustCompile(`^[A-Za-z0-9-]{1,32}$`)

// maxPending is keyclient's limit on the size of a pending file.
const maxPending = 64 << 10

// pendingFields is the part of keyclient's pending file this reader decodes.
// The file is keyclient's internal format (request/request.go, pendingFile).
// This reader is a stop-gap until keyclient exports one. The struct has no
// field for the poll secret, the request id or the poll URL, so they cannot
// be shown by accident.
type pendingFields struct {
	ServiceURL string    `json:"service_url"`
	Override   string    `json:"override"`
	UserCode   string    `json:"user_code"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func pendingErr(path, reason string) error {
	return fmt.Errorf("pending file %s: %s", path, reason)
}

// ReadPending reads the pending file beside tokenPath without a lock and
// without the network. It returns nil, nil when there is none. An error never
// holds file content: the reasons are fixed texts or the operating system's
// text for the path.
func ReadPending(tokenPath string) (*PendingInfo, error) {
	path := request.PendingPath(tokenPath)
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, pendingErr(path, osReason(err))
	}
	if !fi.Mode().IsRegular() {
		return nil, pendingErr(path, "not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, pendingErr(path, osReason(err))
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPending+1))
	if err != nil {
		return nil, pendingErr(path, osReason(err))
	}
	if len(b) > maxPending {
		return nil, pendingErr(path, "larger than 64 KiB")
	}
	var pf pendingFields
	if json.Unmarshal(b, &pf) != nil {
		return nil, pendingErr(path, "not valid JSON of the expected shape")
	}
	info := &PendingInfo{Service: serviceHost(pf.ServiceURL, pf.Override), Code: "(invalid)", Expires: pf.ExpiresAt.UTC()}
	if reUserCode.MatchString(pf.UserCode) {
		info.Code = pf.UserCode
	}
	return info, nil
}

func osReason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return "cannot be read"
}
