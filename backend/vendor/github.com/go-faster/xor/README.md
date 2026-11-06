# xor [![Go Reference](https://img.shields.io/badge/go-pkg-00ADD8)](https://pkg.go.dev/github.com/go-faster/xor#section-documentation) [![codecov](https://img.shields.io/codecov/c/github/go-faster/xor?label=cover)](https://codecov.io/gh/go-faster/xor) [![stable](https://img.shields.io/badge/-stable-brightgreen)](https://go-faster.org/docs/projects/status#stable)

Package xor implements XOR operations on byte slices.
Extracted from [crypto/cipher](https://golang.org/src/crypto/cipher/xor_generic.go).
```console
go get github.com/go-faster/xor
```
```go
xor.Bytes(dst, a, b)
```
**Ref:** [#30553](https://github.com/golang/go/issues/30553) as rejected proposal to provide XOR in go stdlib
