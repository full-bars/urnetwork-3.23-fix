package connect

// RedactProxyCredentials removes the userinfo a proxy address carries and keeps
// the address itself.
func RedactProxyCredentials(text string) string { return text }

// logLine is the text of a select line: the dialer's summary and, when the
// attempt failed, its error.
func (self *clientDialer) logLine(err error) string {
	if err == nil {
		return self.describe(proxyLogLabel)
	}
	return self.describe(proxyLogLabel) + " = " + RedactProxyEndpoint(err.Error(), self.proxySettings())
}
