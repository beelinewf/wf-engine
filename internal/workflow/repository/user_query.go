package repository

import "strings"

// UserListQuery is the parsed list/filter/pagination query for the user
// directory. Order is created_at with an optional leading "-" for
// descending; callers validate before use. Nil bool pointers leave the
// listing unfiltered; empty strings and empty slices do the same.
type UserListQuery struct {
	Page          int
	PerPage       int
	Order         string
	IDs           []string
	Roles         []string
	Issuers       []string
	IsLocal       *bool
	EmailVerified *bool
	IsActive      *bool
	Email         string
	Name          string
}

var userOrderFields = map[string]bool{
	"created_at": true,
}

// userOrderSQL translates an allowlisted order expression into an ORDER
// BY clause. It returns an empty string for anything not allowlisted.
func userOrderSQL(order string) string {
	return orderSQL(order, userOrderFields)
}

// likeSubstring wraps a search term for a case-insensitive substring
// match, escaping the LIKE metacharacters so a literal % or _ in the
// term matches itself rather than everything.
func likeSubstring(term string) string {
	escaped := strings.ReplaceAll(term, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `%`, `\%`)
	escaped = strings.ReplaceAll(escaped, `_`, `\_`)
	return "%" + escaped + "%"
}
