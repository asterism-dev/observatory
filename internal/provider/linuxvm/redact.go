package linuxvm

import "regexp"

var egressSecrets = regexp.MustCompile(`(\s)(vars|account)=\S*`)

// RedactEgress drops the secret names (vars=) and account names (account=) from a proxy log line.
func RedactEgress(line string) string { return egressSecrets.ReplaceAllString(line, "$1$2=") }
