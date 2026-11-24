package policy

// Resource wildcards stay within one ":"-separated part; action wildcards may cross ":".
const (
	resourceSep = ':'
	noSep       = 0
)

func MatchResource(pattern, id string) bool   { return match(pattern, id, resourceSep) }
func MatchAction(pattern, action string) bool { return match(pattern, action, noSep) }

// CoversResource reports whether every identifier granted matches is also matched by held.
func CoversResource(held, granted string) bool { return covers(held, granted, resourceSep) }
func CoversAction(held, granted string) bool   { return covers(held, granted, noSep) }

// OverlapsResource reports whether some identifier matches both patterns.
func OverlapsResource(a, b string) bool { return overlaps(a, b, resourceSep) }
func OverlapsAction(a, b string) bool   { return overlaps(a, b, noSep) }

func wild(r rune) bool { return r == '*' || r == '?' }

// table runs a memoised recursion over positions (i, j) of two rune slices.
func table(a, b []rune, f func(i, j int, rec func(i, j int) bool) bool) bool {
	memo := make([]int8, (len(a)+1)*(len(b)+1))
	var rec func(i, j int) bool
	rec = func(i, j int) bool {
		k := i*(len(b)+1) + j
		if memo[k] == 0 {
			memo[k] = 2
			if f(i, j, rec) {
				memo[k] = 1
			}
		}
		return memo[k] == 1
	}
	return rec(0, 0)
}

func match(pattern, s string, sep rune) bool {
	p, t := []rune(pattern), []rune(s)
	return table(p, t, func(i, j int, rec func(int, int) bool) bool {
		if i == len(p) {
			return j == len(t)
		}
		more := j < len(t) && t[j] != sep
		switch p[i] {
		case '*':
			return rec(i+1, j) || more && rec(i, j+1)
		case '?':
			return more && rec(i+1, j+1)
		default:
			return j < len(t) && p[i] == t[j] && rec(i+1, j+1)
		}
	})
}

// covers is sound, not complete: when it cannot prove inclusion symbol by symbol it answers false.
func covers(held, granted string, sep rune) bool {
	h, g := []rune(held), []rune(granted)
	return table(h, g, func(i, j int, rec func(int, int) bool) bool {
		if i == len(h) {
			return j == len(g)
		}
		switch h[i] {
		case '*':
			// A granted wildcard never produces sep, so a held * may absorb it.
			return rec(i+1, j) || j < len(g) && g[j] != sep && rec(i, j+1)
		case '?':
			return j < len(g) && g[j] != '*' && g[j] != sep && rec(i+1, j+1)
		default:
			return j < len(g) && !wild(g[j]) && h[i] == g[j] && rec(i+1, j+1)
		}
	})
}

func overlaps(x, y string, sep rune) bool {
	a, b := []rune(x), []rune(y)
	return table(a, b, func(i, j int, rec func(int, int) bool) bool {
		if i == len(a) && j == len(b) {
			return true
		}
		if i < len(a) && a[i] == '*' && (rec(i+1, j) || j < len(b) && b[j] != sep && rec(i, j+1)) {
			return true
		}
		if j < len(b) && b[j] == '*' && (rec(i, j+1) || i < len(a) && a[i] != sep && rec(i+1, j)) {
			return true
		}
		if i == len(a) || j == len(b) || a[i] == '*' || b[j] == '*' {
			return false
		}
		ok := a[i] == b[j] ||
			a[i] == '?' && b[j] != sep ||
			b[j] == '?' && a[i] != sep
		return ok && rec(i+1, j+1)
	})
}
