package cli

import (
	"encoding/json"
	"io"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
)

// The JSON form of the subcommands' output (`--json`): one object per call,
// on one line. Lists sit under a named key, never as a bare array.

// WriteJSON writes v as one JSON value on one line.
func WriteJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ErrorBody is the error object: {"error":{"code":"...","message":"..."}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inside of ErrorBody.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError writes the error object.
func WriteError(w io.Writer, code, message string) error {
	return WriteJSON(w, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}

// HostsJSON is the JSON form of HostsTable: {"hosts":[HostView...]}.
func HostsJSON(w io.Writer, hosts []pw.HostView) error {
	if hosts == nil {
		hosts = []pw.HostView{}
	}
	return WriteJSON(w, struct {
		Hosts []pw.HostView `json:"hosts"`
	}{hosts})
}

// ClaimsJSON is the JSON form of ClaimsTable: {"claims":[Claim...]}.
func ClaimsJSON(w io.Writer, claims []pw.Claim) error {
	if claims == nil {
		claims = []pw.Claim{}
	}
	return WriteJSON(w, struct {
		Claims []pw.Claim `json:"claims"`
	}{claims})
}
