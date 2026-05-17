package server

import (
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func hasAdminRole(role string) bool {
	r := roleFromString(role)
	return r == memqlv1.UserRole_USER_ROLE_OWNER || r == memqlv1.UserRole_USER_ROLE_ADMIN
}

func roleFromString(role string) memqlv1.UserRole {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "owner":
		return memqlv1.UserRole_USER_ROLE_OWNER
	case "admin":
		return memqlv1.UserRole_USER_ROLE_ADMIN
	case "writer":
		return memqlv1.UserRole_USER_ROLE_WRITER
	case "reader":
		return memqlv1.UserRole_USER_ROLE_READER
	default:
		return memqlv1.UserRole_USER_ROLE_UNSPECIFIED
	}
}

// HasAdminRole exposes whether the caller possesses at least admin role (owner or admin).
func HasAdminRole(role string) bool {
	return hasAdminRole(role)
}
