package urnettools

import (
	"strconv"
	"strings"
)

// openbsdManagedPID identifies a service only when rc.d's saved matching
// expression selects exactly one process. Ambiguous or malformed data fails closed.
func openbsdManagedPID(runfile string, lookup func(string, string) ([]byte, error)) int {
	values := make(map[string]string)
	for _, line := range strings.Split(runfile, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "pexp" && key != "daemon_rtable") {
			continue
		}
		if _, exists := values[key]; exists {
			return 0
		}
		values[key] = value
	}
	pattern, table := values["pexp"], values["daemon_rtable"]
	if pattern == "" || strings.ContainsAny(pattern, "\r\n") || table == "" {
		return 0
	}
	if _, err := strconv.ParseUint(table, 10, 31); err != nil {
		return 0
	}
	out, err := lookup(table, pattern)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 1 {
		return 0
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
