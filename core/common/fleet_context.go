package common

import (
	"context"
	"strings"
)

type fleetRegistrationKey struct{}

// ContextWithFleetRegistration scopes a model call to one fleet machine the
// caller may use: one they own, or one lent to them (memql#5662). The receiving
// transport binds this after decoding each request; it is never stored on a
// stream or provider shared by other calls. Empty clears the pin.
func ContextWithFleetRegistration(ctx context.Context, registrationId string) context.Context {
	return context.WithValue(ctx, fleetRegistrationKey{}, strings.TrimSpace(registrationId))
}

// FleetRegistrationFromContext returns the strict machine pin, or empty.
func FleetRegistrationFromContext(ctx context.Context) string {
	registrationId, _ := ctx.Value(fleetRegistrationKey{}).(string)
	return registrationId
}
