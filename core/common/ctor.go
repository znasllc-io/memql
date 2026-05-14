package common

type (
	ComponentName string
	Config        any
)

type CtorArg[T any] interface {
	Apply(*T)
	Key() string
	Optional() bool
}

type ctorArgImpl[T any] struct {
	identifier string
	isOptional bool
	fn         func(*T)
}

func (arg ctorArgImpl[T]) Apply(cfg *T) {
	if arg.fn == nil || cfg == nil {
		return
	}
	arg.fn(cfg)
}

func (arg ctorArgImpl[T]) Key() string {
	return arg.identifier
}

func (arg ctorArgImpl[T]) Optional() bool {
	return arg.isOptional
}

func NewCtorArg[T any](name string, optional bool, fn func(*T)) CtorArg[T] {
	return ctorArgImpl[T]{
		identifier: name,
		isOptional: optional,
		fn:         fn,
	}
}
