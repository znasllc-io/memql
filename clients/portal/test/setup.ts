// vitest setup: run once per test file, before any test.
//
// React 19 + @testing-library/react need IS_REACT_ACT_ENVIRONMENT so state
// updates inside render/fireEvent are wrapped in act() rather than warning on
// every assertion.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
