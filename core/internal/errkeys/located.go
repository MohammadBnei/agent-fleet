package errkeys

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
)

// located is err with the file:line that built it (Connect's caller), for the access log's source field: the
// trace id says which request, this says which return. Its words are err's, so the wire is unchanged.
type located struct {
	err   error
	where string
}

func (l located) Error() string { return l.err.Error() }

func (l located) Unwrap() error { return l.err }

// locate wraps err with its caller's caller's location (frame 2: Connect's caller), the path trimmed at the first "/core/"
// (a checkout, a container's /src or -trimpath's module path all hold one) so the field reads internal/x/y.go:N.
func locate(err error) error {
	if err == nil {
		return nil
	}
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		return err
	}
	if i := strings.Index(file, "/core/"); i >= 0 {
		file = file[i+len("/core/"):]
	}
	return located{err, file + ":" + strconv.Itoa(line)}
}

// LocationOf is the file:line an errkeys.Connect error was built at, or "" for one built otherwise (a gRPC status,
// a sentinel kept on connect.NewError, an error Connect raised itself).
func LocationOf(err error) string {
	var l located
	if errors.As(err, &l) {
		return l.where
	}
	return ""
}
