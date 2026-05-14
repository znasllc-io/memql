package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	memoryNodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"github.com/visionarys-io/memql/component/memql"
)

var (
	// ErrUserNotFound indicates that the requested user does not exist.
	ErrUserNotFound = errors.New("user not found")

	// ErrUserInvalid indicates that the provided payload is invalid.
	ErrUserInvalid = errors.New("invalid payload")
)

type (
	// User represents a user instance.
	User struct {
		ID          string
		Email       string
		PhoneNumber string
		Role        string
	}

	// UserRecord represents a stored user along with memory node metadata.
	UserRecord struct {
		User      User
		ID        string
		Subject   string
		CreatedAt time.Time
		CreatedBy string
		Schema    map[string]any
	}

	// UserService defines operations available for managing users.
	UserService interface {
		GetUserByEmail(ctx context.Context, email string) (*User, error)
		GetUserById(ctx context.Context, userId string) (*User, error)
		CreateUser(ctx context.Context, email, phoneNumber, role string) (*User, error)
		EnsureUser(ctx context.Context, email, phoneNumber, role string) (*User, error)
	}

	// UserMemoryNodeStore abstracts persistence operations for concept handlers.
	UserMemoryNodeStore interface {
		memoryNodes.Store
	}

	userMemoryNodeService struct {
		store        UserMemoryNodeStore
		memoryEngine *memql.MemQLEngine
		userConcept  *memoryNodes.Concept
	}
)

// NewUserService creates a new user service using the provided store.
func NewUserService(ctx context.Context, store UserMemoryNodeStore, memoryEngine *memql.MemQLEngine) (UserService, error) {
	if store == nil {
		return nil, fmt.Errorf("memory node store is required")
	}
	if memoryEngine == nil {
		return nil, fmt.Errorf("memory engine is required")
	}

	concept, err := memoryNodes.Get(memoryNodes.ConceptUser)
	if err != nil {
		return nil, fmt.Errorf("resolve user concept %q: %w", memoryNodes.ConceptUser, err)
	}

	return &userMemoryNodeService{
		store:        store,
		memoryEngine: memoryEngine,
		userConcept:  concept,
	}, nil
}

// GetUserByEmail retrieves the latest version of a user by their email address.
func (s *userMemoryNodeService) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	if strings.TrimSpace(email) == "" {
		return nil, fmt.Errorf("email is required")
	}

	// Query using MemQL to get the most recent version (time-series model)
	query := fmt.Sprintf(`concept==v1:memql:backend:user;payload.email=="%s"`, strings.TrimSpace(email))

	result, err := s.memoryEngine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query user by email: %w", err)
	}

	node := firstConceptNode(result, memoryNodes.ConceptUser)
	if node == nil {
		return nil, ErrUserNotFound
	}

	return userFromNode(node)
}

// GetUserById retrieves the latest version of a user by their ID.
func (s *userMemoryNodeService) GetUserById(ctx context.Context, userId string) (*User, error) {
	if strings.TrimSpace(userId) == "" {
		return nil, fmt.Errorf("user ID is required")
	}

	// Query using MemQL to get the most recent version (time-series model)
	query := fmt.Sprintf(`concept==v1:memql:backend:user;id=="%s"`, strings.TrimSpace(userId))

	result, err := s.memoryEngine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query user by ID: %w", err)
	}

	node := firstConceptNode(result, memoryNodes.ConceptUser)
	if node == nil {
		return nil, ErrUserNotFound
	}

	return userFromNode(node)
}

// CreateUser creates a new user with the provided email, phone number, and role.
// If a user with the same email already exists, returns the existing user without creating a new version.
func (s *userMemoryNodeService) CreateUser(ctx context.Context, email, phoneNumber, role string) (*User, error) {
	email = strings.TrimSpace(email)
	phoneNumber = strings.TrimSpace(phoneNumber)
	role = strings.TrimSpace(role)

	if err := validateUserParams(email, phoneNumber, role); err != nil {
		return nil, err
	}

	if role == "" {
		role = "user" // Default role
	}

	// Check if user already exists
	existing, err := s.GetUserByEmail(ctx, email)
	if err == nil && existing != nil {
		return existing, nil // User already exists
	}

	// Insert new user
	return s.insertUser(ctx, email, phoneNumber, role)
}

// insertUser inserts a new user record (or new version) without checking for existence.
// This follows the time-series model where "updates" are new inserts with the same ID.
func (s *userMemoryNodeService) insertUser(ctx context.Context, email, phoneNumber, role string) (*User, error) {
	email = strings.TrimSpace(email)
	phoneNumber = strings.TrimSpace(phoneNumber)
	role = strings.TrimSpace(role)

	if err := validateUserParams(email, phoneNumber, role); err != nil {
		return nil, err
	}

	if role == "" {
		role = "user" // Default role
	}

	// Use email as the user ID (consistent across versions)
	userId := email

	// Create payload
	userPayload := map[string]any{
		"email":       email,
		"phoneNumber": phoneNumber,
		"role":        strings.ToLower(role),
	}

	payloadJSON, err := json.Marshal(userPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal user payload: %w", err)
	}

	// Insert using MemQL - this creates a new version in the time-series
	insertQuery := fmt.Sprintf(`insert("%s", id="%s", payload=%s)`, memoryNodes.ConceptMemQLBackendUser, userId, string(payloadJSON))

	result, err := s.memoryEngine.Execute(ctx, insertQuery)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	node := firstConceptNode(result, memoryNodes.ConceptUser)
	if node == nil {
		return nil, fmt.Errorf("user insert did not return a node")
	}

	return userFromNode(node)
}

// validateUserParams validates user creation/update parameters.
// Email is required; phone number is optional (kept for potential future format validation).
func validateUserParams(email, _ /* phoneNumber */, role string) error {
	if strings.TrimSpace(email) == "" {
		return fmt.Errorf("email is required")
	}

	role = strings.TrimSpace(role)
	if role == "" {
		return nil // Will use default
	}

	// Validate role
	validRoles := map[string]bool{
		"owner":     true,
		"developer": true,
		"admin":     true,
		"user":      true,
	}
	if !validRoles[strings.ToLower(role)] {
		return fmt.Errorf("invalid role: %s", role)
	}

	return nil
}

// EnsureUser ensures a user exists with the current data, creating or updating as necessary.
// Following the time-series model:
// - If user doesn't exist: creates a new user record
// - If user exists but data changed (role, phoneNumber): inserts a new version
// - If user exists with same data: returns existing user (no new insert)
func (s *userMemoryNodeService) EnsureUser(ctx context.Context, email, phoneNumber, role string) (*User, error) {
	email = strings.TrimSpace(email)
	phoneNumber = strings.TrimSpace(phoneNumber)
	role = strings.TrimSpace(role)

	if role == "" {
		role = "user" // Default role
	}
	role = strings.ToLower(role)

	// Try to get existing user (latest version)
	existing, err := s.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// User doesn't exist, create it
			return s.insertUser(ctx, email, phoneNumber, role)
		}
		return nil, fmt.Errorf("check existing user: %w", err)
	}

	// User exists - check if any data has changed
	if userDataChanged(existing, phoneNumber, role) {
		// Data changed - insert new version (time-series "update")
		return s.insertUser(ctx, email, phoneNumber, role)
	}

	// No changes, return existing user
	return existing, nil
}

// userDataChanged compares the existing user data with new values.
// Returns true if any field has changed and a new version should be inserted.
func userDataChanged(existing *User, newPhoneNumber, newRole string) bool {
	if existing == nil {
		return true
	}

	// Compare phone number
	if strings.TrimSpace(existing.PhoneNumber) != strings.TrimSpace(newPhoneNumber) {
		return true
	}

	// Compare role (case-insensitive)
	if !strings.EqualFold(strings.TrimSpace(existing.Role), strings.TrimSpace(newRole)) {
		return true
	}

	return false
}

func userFromNode(node *memqlv1.MemoryNode) (*User, error) {
	if node == nil {
		return nil, fmt.Errorf("memory node is nil")
	}
	if strings.TrimSpace(node.Id) == "" {
		return nil, fmt.Errorf("user memory node missing id")
	}

	payloadStruct := node.GetPayload()
	if payloadStruct == nil {
		return nil, fmt.Errorf("user payload is nil")
	}
	payload := payloadStruct.AsMap()

	email := userStringFromAny(payload["email"])
	phoneNumber := userStringFromAny(payload["phoneNumber"])
	role := userStringFromAny(payload["role"])

	if email == "" {
		return nil, fmt.Errorf("user payload missing email")
	}
	if phoneNumber == "" {
		return nil, fmt.Errorf("user payload missing phoneNumber")
	}
	if role == "" {
		role = "user" // Default role
	}

	return &User{
		ID:          strings.TrimSpace(node.Id),
		Email:       email,
		PhoneNumber: phoneNumber,
		Role:        role,
	}, nil
}

func userStringFromAny(v any) string {
	if v == nil {
		return ""
	}
	str, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(str)
}

func firstConceptNode(result *memql.ExecuteResult, conceptName string) *memqlv1.MemoryNode {
	if result == nil || result.Bundle == nil {
		return nil
	}
	for i := range result.Bundle.Nodes {
		node := result.Bundle.Nodes[i]
		if node == nil {
			continue
		}
		if strings.EqualFold(node.Concept, conceptName) {
			return node
		}
	}
	return nil
}
