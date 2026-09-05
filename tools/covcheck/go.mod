// covcheck lives in its own module on purpose.
//
// Two reasons. The binary's rule is standard library only (CLAUDE.md), and a
// nested module keeps that provable: `go list -deps ./...` in the root module
// never reaches this directory. And coverage is measured over the root module's
// packages, so tooling that would never be exercised by kura's own tests must
// not sit in the denominator — a 100% target is meaningless if it counts code
// that exists only to measure coverage.
module kura/tools/covcheck

go 1.26
