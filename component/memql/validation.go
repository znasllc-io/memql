package memql

import "log/slog"

// ValidationReport summarizes boot-time validation results for all MemQL components.
// This provides a structured view of system health at startup.
type ValidationReport struct {
	Concepts    ValidationSection
	Functions   ValidationSection
	Automations ValidationSection
	Specs       ValidationSection
	Shapes      ValidationSection
	Providers   ValidationSection
}

// ValidationSection tracks loading statistics for a single component type.
type ValidationSection struct {
	Loaded  int
	Skipped int
	Errors  []ValidationError
}

// ValidationError describes a specific validation failure.
type ValidationError struct {
	Name  string
	File  string
	Error string
}

// NewValidationReport creates an empty validation report.
func NewValidationReport() *ValidationReport {
	return &ValidationReport{
		Concepts:    ValidationSection{Errors: []ValidationError{}},
		Functions:   ValidationSection{Errors: []ValidationError{}},
		Automations: ValidationSection{Errors: []ValidationError{}},
		Specs:       ValidationSection{Errors: []ValidationError{}},
		Shapes:      ValidationSection{Errors: []ValidationError{}},
		Providers:   ValidationSection{Errors: []ValidationError{}},
	}
}

// TotalErrors returns the total number of errors across all sections.
func (r *ValidationReport) TotalErrors() int {
	return len(r.Concepts.Errors) +
		len(r.Functions.Errors) +
		len(r.Automations.Errors) +
		len(r.Specs.Errors) +
		len(r.Shapes.Errors) +
		len(r.Providers.Errors)
}

// TotalSkipped returns the total number of skipped items across all sections.
func (r *ValidationReport) TotalSkipped() int {
	return r.Concepts.Skipped +
		r.Functions.Skipped +
		r.Automations.Skipped +
		r.Specs.Skipped +
		r.Shapes.Skipped +
		r.Providers.Skipped
}

// Status returns "healthy" if no errors, "degraded" if there are skipped items, "unhealthy" if there are errors.
func (r *ValidationReport) Status() string {
	if r.TotalErrors() > 0 {
		return "unhealthy"
	}
	if r.TotalSkipped() > 0 {
		return "degraded"
	}
	return "healthy"
}

// LogSummary emits a structured log entry summarizing the validation report.
func (r *ValidationReport) LogSummary(logger *slog.Logger) {
	if logger == nil {
		return
	}

	status := r.Status()
	totalErrors := r.TotalErrors()
	totalSkipped := r.TotalSkipped()

	logger.Info("boot validation complete",
		"component", ComponentName,
		"status", status,
		"concepts", r.Concepts.Loaded,
		"functions", r.Functions.Loaded,
		"functionsSkipped", r.Functions.Skipped,
		"automations", r.Automations.Loaded,
		"automationsSkipped", r.Automations.Skipped,
		"specs", r.Specs.Loaded,
		"shapes", r.Shapes.Loaded,
		"providers", r.Providers.Loaded,
		"totalErrors", totalErrors,
		"totalSkipped", totalSkipped,
	)

	// Log individual errors if present
	if totalErrors > 0 {
		for _, err := range r.Functions.Errors {
			logger.Error("function validation failed",
				"component", ComponentName,
				"name", err.Name,
				"file", err.File,
				"error", err.Error,
			)
		}
		for _, err := range r.Automations.Errors {
			logger.Error("automation validation failed",
				"component", ComponentName,
				"name", err.Name,
				"file", err.File,
				"error", err.Error,
			)
		}
		for _, err := range r.Concepts.Errors {
			logger.Error("concept validation failed",
				"component", ComponentName,
				"name", err.Name,
				"file", err.File,
				"error", err.Error,
			)
		}
		for _, err := range r.Specs.Errors {
			logger.Error("spec validation failed",
				"component", ComponentName,
				"name", err.Name,
				"file", err.File,
				"error", err.Error,
			)
		}
		for _, err := range r.Shapes.Errors {
			logger.Error("shape validation failed",
				"component", ComponentName,
				"name", err.Name,
				"file", err.File,
				"error", err.Error,
			)
		}
		for _, err := range r.Providers.Errors {
			logger.Error("provider validation failed",
				"component", ComponentName,
				"name", err.Name,
				"file", err.File,
				"error", err.Error,
			)
		}
	}
}

// AddFunctionError records a function validation error.
func (r *ValidationReport) AddFunctionError(name, file, err string) {
	r.Functions.Errors = append(r.Functions.Errors, ValidationError{
		Name:  name,
		File:  file,
		Error: err,
	})
	r.Functions.Skipped++
}

// AddAutomationError records an automation validation error.
func (r *ValidationReport) AddAutomationError(name, file, err string) {
	r.Automations.Errors = append(r.Automations.Errors, ValidationError{
		Name:  name,
		File:  file,
		Error: err,
	})
	r.Automations.Skipped++
}
