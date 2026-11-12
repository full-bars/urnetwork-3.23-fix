package urnettools

import (
	"strconv"
	"strings"
)

type openBSDProcess struct {
	pid int
	uid string
}

func parseOpenBSDProcesses(output string) []openBSDProcess {
	var rows []openBSDProcess
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !isExactProviderBinary(fields[2]) {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		if _, err := strconv.ParseUint(fields[1], 10, 32); err != nil {
			continue
		}
		rows = append(rows, openBSDProcess{pid: pid, uid: fields[1]})
	}
	return rows
}
