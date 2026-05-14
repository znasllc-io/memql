package language

import (
	"log/slog"
	"os"
	"strings"

	"github.com/visionarys-io/memql/component"
	"github.com/visionarys-io/memql/component/language/compiler"
	"github.com/visionarys-io/memql/component/language/parser"
	"github.com/visionarys-io/memql/core/common"
	"github.com/visionarys-io/memql/core/env"
	"github.com/visionarys-io/memql/core/logger"
)

const (
	// ComponentName identifies the combined parser/compiler pipeline component.
	ComponentName = common.ComponentName("language")
	// ParserComponentName identifies the parser submodule for logging configuration.
	ParserComponentName = common.ComponentName("parser")
	// CompilerComponentName identifies the compiler submodule for logging configuration.
	CompilerComponentName = common.ComponentName("compiler")
)

// Language coordinates the parser and compiler submodules under a single lifecycle.
type Language struct {
	*component.Component
	parser   *ParserModule
	compiler *CompilerModule
}

// New constructs a component that exposes parser and compiler submodules.
// The component itself participates in the dependency lifecycle via component.Component,
// while each submodule maintains its own logger configured via environment variables.
func New(args ...component.Arg) (*Language, error) {
	base, err := component.New(ComponentName, args...)
	if err != nil {
		return nil, err
	}

	return &Language{
		Component: base,
		parser:    &ParserModule{logger: newParserLogger()},
		compiler:  &CompilerModule{logger: newCompilerLogger()},
	}, nil
}

// Parser returns the parser submodule, if available.
func (c *Language) Parser() *ParserModule {
	return c.parser
}

// Compiler returns the compiler submodule, if available.
func (c *Language) Compiler() *CompilerModule {
	return c.compiler
}

// ParserModule provides constructor helpers for parser primitives alongside a configurable logger.
type ParserModule struct {
	logger *slog.Logger
}

// Logger exposes the parser module logger configured via environment variables.
func (m *ParserModule) Logger() *slog.Logger {
	return m.logger
}

// NewLexer creates a lexer for the provided input.
func (m *ParserModule) NewLexer(input string) *parser.Lexer {
	return parser.NewLexer(input)
}

// NewParser creates a parser for the provided tokens.
func (m *ParserModule) NewParser(tokens []parser.Token) *parser.Parser {
	return parser.NewParser(tokens)
}

// CompilerModule provides constructor helpers for compiler instances alongside a configurable logger.
type CompilerModule struct {
	logger *slog.Logger
}

// Logger exposes the compiler module logger configured via environment variables.
func (m *CompilerModule) Logger() *slog.Logger {
	return m.logger
}

// New creates a compiler instance with the provided configuration.
func (m *CompilerModule) New(cfg compiler.Config) *compiler.Compiler {
	return compiler.New(cfg)
}

// NewDefault creates a compiler instance with the default configuration.
func (m *CompilerModule) NewDefault() *compiler.Compiler {
	return compiler.NewDefault()
}

func newParserLogger() *slog.Logger {
	return logger.New(ParserComponentName, os.Stdout, resolveLoggerLevel(ParserComponentName, slog.LevelInfo))
}

func newCompilerLogger() *slog.Logger {
	return logger.New(CompilerComponentName, os.Stdout, resolveLoggerLevel(CompilerComponentName, slog.LevelInfo))
}

func resolveLoggerLevel(component common.ComponentName, fallback slog.Level) slog.Level {
	prefix := env.EnvPrefixForComponent(component)
	if prefix == "" {
		return fallback
	}

	reader := env.NewEnvReader(prefix)
	for _, key := range loggerLevelKeys() {
		value, ok := reader.String(key)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}

		var level slog.Level
		if err := level.UnmarshalText([]byte(strings.ToLower(value))); err == nil {
			return level
		}
	}

	return fallback
}

func loggerLevelKeys() []string {
	return []string{
		"CAPABILITIES_LOGGING_LOG_LEVEL",
		"LOGGER_LEVEL",
		"LOG_LEVEL",
	}
}
