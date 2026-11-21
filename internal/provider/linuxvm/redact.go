package linuxvm

import "regexp"

var (
	egressVars    = regexp.MustCompile(`(\s)vars=\S*`)
	egressAccount = regexp.MustCompile(`(\s)account=.*?( limited until=|$)`)
)

// RedactEgress drops the secret names (vars=) and account names (account=, which may contain spaces) from a proxy log line.
func RedactEgress(line string) string {
	return egressAccount.ReplaceAllString(egressVars.ReplaceAllString(line, "${1}vars="), "${1}account=$2")
}
