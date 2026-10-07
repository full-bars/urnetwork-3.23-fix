package urnettools

import (
	"strconv"
	"strings"
)

// parseProcstatBasic turns `procstat -b` output into one record per process.
//
// FreeBSD's procstat prints a header line, then "PID COMM OSREL PATH" rows,
// and marks unreadable fields with "-" (a process this user may not inspect,
// or one that exited between the enumeration and the read). The parser keeps
// only rows where the PATH is a provider binary and the PID parsed, and it
// deliberately keeps the rows where PATH is "-" OUT: argv[0] alone is not
// enough to trust a row, because a process can rename itself with `exec -a`
// and would then be indistinguishable from a real provider by name alone. The
// binary path is what the kernel reports for the loaded image.
//
// This is a pure function so the parsing rules can be tested on any platform.
func parseProcstatBasic(out string) []procstatProc {
	var rows []procstatProc
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// Header row ("PID COMM OSREL PATH") and any future wider format are
		// skipped by requiring a numeric first field and at least 4 columns.
		if len(fields) < 4 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			continue // header or malformed row
		}
		rows = append(rows, procstatProc{
			pid:  fields[0],
			comm: fields[1],
			path: fields[len(fields)-1],
		})
	}
	return rows
}

// procstatProc is one parsed procstat -b row.
type procstatProc struct {
	pid  string
	comm string
	path string
}

// parseProcstatCredentials maps `procstat -s` rows to pid -> (euid, ruid).
//
// The output is a fixed-width table with a header:
//
//	PID COMM EUID RUID SVUID EGID RGID SVGID UMASK FLAGS GROUPS
//
// The EUID/RUID columns are the KERNEL's credential record for the process, not
// anything the process printed about itself, which is why they — and not the
// environ — decide who owns a discovered provider. Values are decimal or the
// literal "-" when unreadable.
func parseProcstatCredentials(out string) map[string]procstatCreds {
	creds := map[string]procstatCreds{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimRight(line, "\r"))
		if len(fields) < 4 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			continue // header
		}
		euid, eok := parseUIDField(fields[2])
		ruid, rok := parseUIDField(fields[3])
		creds[fields[0]] = procstatCreds{euid: euid, ruid: ruid, haveEUID: eok, haveRUID: rok}
	}
	return creds
}

// procstatCreds is the credential pair read for one pid.
type procstatCreds struct {
	euid     uint32
	ruid     uint32
	haveEUID bool
	haveRUID bool
}

// parseUIDField reads one credential column, reporting whether it was a real
// uid. A "-" (unreadable) or a non-numeric cell is not a uid and must never be
// coerced to 0, because 0 is root and treating "unknown" as root would let an
// unprivileged process be managed as the system owner.
func parseUIDField(f string) (uint32, bool) {
	v, err := strconv.ParseUint(f, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}
