package connect

var CritLogger func(format string, args ...any)

func LogCritical(format string, args ...any) {
	if CritLogger != nil {
		CritLogger(format, args...)
	}
}
