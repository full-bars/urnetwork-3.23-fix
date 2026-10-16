package connect

import (
	"fmt"
	"strings"
)

// HttpStatusError carries a non-200 response status so a caller can act on the
// STATUS rather than string-matching an error message. The miner-side retry
// policies (ported with the claim daemons) classify reads on the status code;
// the v33 lineage previously returned an untyped error here.
//
// Error() renders exactly as the untyped error it replaces, so existing
// callers that only log or string-match are unaffected.
type HttpStatusError struct {
	StatusCode int
	Status     string
	Body       []byte
}

func (self *HttpStatusError) Error() string {
	body := strings.TrimSpace(string(self.Body))
	if strings.HasPrefix(body, "<") {
		return fmt.Sprintf("%s: <html error page, %d bytes>", self.Status, len(self.Body))
	}
	return fmt.Sprintf("%s: %s", self.Status, body)
}
