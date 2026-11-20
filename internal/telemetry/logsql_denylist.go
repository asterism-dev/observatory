package telemetry

// LeakingLogsQL lists LogsQL constructs that read past a forced extra_filters scope on VictoriaLogs v1.54.0; vlprobe_test.go keeps it honest.
var LeakingLogsQL = []string{}
