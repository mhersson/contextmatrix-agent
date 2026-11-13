// Package cardid holds the ContextMatrix card ID pattern that work and
// /run-kind both enforce.
package cardid

import "regexp"

// pattern matches ContextMatrix card IDs (PREFIX-NNN, upper- or lower-case): a
// letter-led prefix of letters, digits and dashes (MY-PROJ-001 is legitimate),
// a dash and a numeric suffix, exactly what CM's ID generator produces. The ID
// becomes part of the cm/ work branch name and of host paths, so this
// conservative shape keeps colons, slashes, dots, spaces and leading dashes
// out of both.
var pattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*-[0-9]+$`)

// Valid reports whether id is a ContextMatrix card ID.
func Valid(id string) bool {
	return pattern.MatchString(id)
}
