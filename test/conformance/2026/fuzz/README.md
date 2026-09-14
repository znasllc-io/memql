# Fuzz seeds

`FuzzParse` (`test/conformance/fuzz_test.go`) feeds the edition's front end and
the parser every case file in this edition's corpus, plus every `.memql` file
here, and whatever the fuzzer derives from them. The parser may refuse any
input; it may never panic.

This directory grows only from found failures: when a fuzz run finds an input
that breaks the parser, the input goes here as a `.memql` file named after what
it broke, beside the fix. `go test` then runs it on every build.

To fuzz:

```bash
go test -run '^$' -fuzz FuzzParse ./test/conformance/
```
