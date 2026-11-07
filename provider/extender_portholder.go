package main

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"time"
)

// portHolderPattern picks the first `("name",pid=N` out of ss's users column.
var portHolderPattern = regexp.MustCompile(`\("([^"]+)",pid=(\d+)`)

// parsePortHolder names the process that holds a listener from `ss -ltnpH`
// output, or "" when the output names none. The role uses it to say WHO took
// tcp 443, because "address already in use" alone sends an operator hunting.
func parsePortHolder(ssOutput string) string {
	m := portHolderPattern.FindStringSubmatch(ssOutput)
	if m == nil {
		return ""
	}
	return m[1] + " (pid " + m[2] + ")"
}

// tcpPortHolder asks the host which process listens on a tcp port. Best effort
// and Linux only: it returns "" when ss is missing, times out, or the holder is
// another user's process whose name the kernel will not show. It never blocks
// the role for more than two seconds.
func tcpPortHolder(port string) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ss", "-ltnpH", "sport", "=", ":"+port).Output()
	if err != nil {
		return ""
	}
	return parsePortHolder(string(out))
}
