package auth

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
)

// Role-test principals are real users with a reserved id namespace. They may
// authenticate only through a bounded role-test session, never a login factor.
const RoleTestUserPrefix = "role-test-"
const RoleTestClass = "role_test"

func IsRoleTestUser(id string) bool {
	return strings.HasPrefix(strings.TrimPrefix(id, "v1:identity:user:"), RoleTestUserPrefix)
}

// Exact roles, not ranks: developer and admin have different authority even
// though their ordering rung is the same. No caller can create an owner test.
func MayTestRole(operator, target Role) bool {
	switch target {
	case RoleReader, RoleWriter, RoleDeveloper:
		return operator == RoleOwner || operator == RoleDeveloper
	case RoleAdmin:
		return operator == RoleOwner
	default:
		return false
	}
}

// Every mesh node checks the shared session before executing a forwarded or
// persisted test action. The assertion carries an id, never its bearer secret.
type RoleTestValidator interface {
	ValidateRoleTest(context.Context, string, string, Role) error
}
type roleTestValidatorHolder struct{ validator RoleTestValidator }

var roleTestValidator atomic.Pointer[roleTestValidatorHolder]

func SetRoleTestValidator(v RoleTestValidator) { roleTestValidator.Store(&roleTestValidatorHolder{v}) }
func ValidateRoleTestContext(ctx context.Context) error {
	if ctx.Value(roleTestValidationReadKey{}) == true {
		return nil
	}
	var sessionID, subject string
	var role Role
	if a, ok := ForwardedAuthorityFromContext(ctx); ok && a.CredentialClass == RoleTestClass {
		sessionID, subject, role = a.RoleTestSessionID, a.Subject, a.Role
	} else if c, ok := ClaimsFromContext(ctx); ok && c["class"] == RoleTestClass {
		sessionID, _ = c["role_test_session"].(string)
		subject, _ = c["sub"].(string)
		r, _ := c["role"].(string)
		role = Role(r)
	} else {
		if ac, ok := AccessFromContext(ctx); ok && IsRoleTestUser(ac.UserId) {
			return errors.New("test personas require a bounded role-test authority")
		}
		if c, ok := ClaimsFromContext(ctx); ok {
			if sub, _ := c["sub"].(string); IsRoleTestUser(sub) {
				return errors.New("test personas require a bounded role-test authority")
			}
		}
		return nil
	}
	if ac, ok := AccessFromContext(ctx); ok && (ac.UserId != subject || ac.Role != role) {
		return errors.New("role-test actor differs from its session")
	}
	holder := roleTestValidator.Load()
	if sessionID == "" || holder == nil || holder.validator == nil {
		return errors.New("role-test authority is unavailable")
	}
	return holder.validator.ValidateRoleTest(ctx, sessionID, subject, role)
}

// ContextForRoleTestValidation scopes the trusted identity reads performed by
// the validator itself. Ordinary internal work still checks its test session.
type roleTestValidationReadKey struct{}

func ContextForRoleTestValidation(ctx context.Context) context.Context {
	return context.WithValue(ContextWithInternalOrigin(ctx), roleTestValidationReadKey{}, true)
}

type CheckedMembershipSource interface {
	ActiveGroupIdsForUserChecked(context.Context, string) ([]string, error)
}
