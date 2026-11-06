# jx [![](https://img.shields.io/badge/go-pkg-00ADD8)](https://pkg.go.dev/github.com/go-faster/jx#section-documentation) [![](https://img.shields.io/codecov/c/github/go-faster/jx?label=cover)](https://codecov.io/gh/go-faster/jx) [![stable](https://img.shields.io/badge/-stable-brightgreen)](https://go-faster.org/docs/projects/status#stable)

Package jx implements encoding and decoding of json [[RFC 7159](https://www.rfc-editor.org/rfc/rfc7159.html)].
Lightweight fork of [jsoniter](https://github.com/json-iterator/go).

```console
go get github.com/go-faster/jx
```

* [Usage and examples](#usage)
* [Roadmap](#roadmap)
* [Non-goals](#non-goals)

## Features
* Mostly zero-allocation and highly optimized
* Directly encode and decode json values
* No reflect or `interface{}`
* Pools and direct buffer access for less (or none) allocations
* Multi-pass decoding
* Validation

See [usage](#Usage) for examples. Mostly suitable for fast low-level json manipulation
with high control, for dynamic parsing and encoding of unstructured data. Used in [ogen](https://github.com/ogen-go/ogen) project for
json (un)marshaling code generation based on json and OpenAPI schemas.

For example, we have following OpenTelemetry log entry:

```json
{
  "Timestamp": "1586960586000000000",
  "Attributes": {
    "http.status_code": 500,
    "http.url": "http://example.com",
    "my.custom.application.tag": "hello"
  },
  "Resource": {
    "service.name": "donut_shop",
    "service.version": "2.0.0",
    "k8s.pod.uid": "1138528c-c36e-11e9-a1a7-42010a800198"
  },
  "TraceId": "13e2a0921288b3ff80df0a0482d4fc46",
  "SpanId": "43222c2d51a7abe3",
  "SeverityText": "INFO",
  "SeverityNumber": 9,
  "Body": "20200415T072306-0700 INFO I like donuts"
}
```

Flexibility of `jx` enables highly efficient semantic-aware encoding and decoding,
e.g. using `[16]byte` for `TraceId` with zero-allocation `hex` encoding in json:

| Name     | Speed     | Allocations |
|----------|-----------|-------------|
| Decode   | 1279 MB/s | 0 allocs/op |
| Validate | 1914 MB/s | 0 allocs/op |
| Encode   | 1202 MB/s | 0 allocs/op |
| Write    | 2055 MB/s | 0 allocs/op |

`cpu: AMD Ryzen 9 7950X`

See [otel_test.go](./otel_test.go) for example.

## Why

Most of [jsoniter](https://github.com/json-iterator/go) issues are caused by necessity
to be drop-in replacement for standard `encoding/json`. Removing such constrains greatly
simplified implementation and reduced scope, allowing to focus on json stream processing.

* Commas are handled automatically while encoding
* Raw json, Number and Base64 support
* Reduced scope
  * No reflection
  * No `encoding/json` adapter
  * 3.5x less code (8.5K to 2.4K SLOC)
* Fuzzing, improved test coverage
* Drastically refactored and simplified
  * Explicit error returns
  * No `Config` or `API`


## Usage

* [Decoding](#decode)
* [Encoding](#encode)
* [Writer](#writer)
* [Raw message](#raw)
* [Number](#number)
* [Base64](#base64)
* [Validation](#validate)
* [Multi pass decoding](#capture)

### Decode

Use [jx.Decoder](https://pkg.go.dev/github.com/go-faster/jx#Decoder). Zero value is valid,
but constructors are available for convenience:
  * [jx.Decode(reader io.Reader, bufSize int)](https://pkg.go.dev/github.com/go-faster/jx#Decode) for `io.Reader`
  * [jx.DecodeBytes([]byte)](https://pkg.go.dev/github.com/go-faster/jx#Decode)  for byte slices
  * [jx.DecodeStr(string)](https://pkg.go.dev/github.com/go-faster/jx#Decode) for strings

To reuse decoders and their buffers, use [jx.GetDecoder](https://pkg.go.dev/github.com/go-faster/jx#GetDecoder)
and [jx.PutDecoder](https://pkg.go.dev/github.com/go-faster/jx#PutDecoder) alongside with reset functions:
* [jx.Decoder.Reset(io.Reader)](https://pkg.go.dev/github.com/go-faster/jx#Decoder.Reset) to reset to new `io.Reader`
* [jx.Decoder.ResetBytes([]byte)](https://pkg.go.dev/github.com/go-faster/jx#Decoder.ResetBytes) to decode another byte slice

Decoder is reset on `PutDecoder`.

```go
d := jx.DecodeStr(`{"values":[4,8,15,16,23,42]}`)

// Save all integers from "values" array to slice.
var values []int

// Iterate over each object field.
if err := d.Obj(func(d *jx.Decoder, key string) error {
    switch key {
    case "values":
        // Iterate over each array element.
        return d.Arr(func(d *jx.Decoder) error {
            v, err := d.Int()
            if err != nil {
                return err
            }
            values = append(values, v)
            return nil
        })
    default:
        // Skip unknown fields if any.
        return d.Skip()
    }
}); err != nil {
    panic(err)
}

fmt.Println(values)
// Output: [4 8 15 16 23 42]
```

### Encode
Use [jx.Encoder](https://pkg.go.dev/github.com/go-faster/jx#Encoder). Zero value is valid, reuse with
[jx.GetEncoder](https://pkg.go.dev/github.com/go-faster/jx#GetEncoder),
[jx.PutEncoder](https://pkg.go.dev/github.com/go-faster/jx#PutEncoder) and
[jx.Encoder.Reset()](https://pkg.go.dev/github.com/go-faster/jx#Encoder.Reset). Encoder is reset on `PutEncoder`.
```go
var e jx.Encoder
e.ObjStart()           // {
e.FieldStart("values") // "values":
e.ArrStart()           // [
for _, v := range []int{4, 8, 15, 16, 23, 42} {
    e.Int(v)
}
e.ArrEnd() // ]
e.ObjEnd() // }
fmt.Println(e)
fmt.Println("Buffer len:", len(e.Bytes()))
// Output: {"values":[4,8,15,16,23,42]}
// Buffer len: 28
```

### Writer

Use [jx.Writer](https://pkg.go.dev/github.com/go-faster/jx#Writer) for low level json writing.

No automatic commas or indentation for lowest possible overhead, useful for code generated json encoding.

### Raw
Use [jx.Decoder.Raw](https://pkg.go.dev/github.com/go-faster/jx#Decoder.Raw) to read raw json values, similar to `json.RawMessage`.
```go
d := jx.DecodeStr(`{"foo": [1, 2, 3]}`)

var raw jx.Raw
if err := d.Obj(func(d *jx.Decoder, key string) error {
    v, err := d.Raw()
    if err != nil {
        return err
    }
    raw = v
    return nil
}); err != nil {
    panic(err)
}

fmt.Println(raw.Type(), raw)
// Output:
// array [1, 2, 3]
```

### Number

Use [jx.Decoder.Num](https://pkg.go.dev/github.com/go-faster/jx#Decoder.Num) to read numbers, similar to `json.Number`.
Also supports number strings, like `"12345"`, which is common compatible way to represent `uint64`.

```go
d := jx.DecodeStr(`{"foo": "10531.0"}`)

var n jx.Num
if err := d.Obj(func(d *jx.Decoder, key string) error {
    v, err := d.Num()
    if err != nil {
        return err
    }
    n = v
    return nil
}); err != nil {
    panic(err)
}

fmt.Println(n)
fmt.Println("positive:", n.Positive())

// Can decode floats with zero fractional part as integers:
v, err := n.Int64()
if err != nil {
    panic(err)
}
fmt.Println("int64:", v)
// Output:
// "10531.0"
// positive: true
// int64: 10531
```

### Base64
Use [jx.Encoder.Base64](https://pkg.go.dev/github.com/go-faster/jx#Encoder.Base64) and
[jx.Decoder.Base64](https://pkg.go.dev/github.com/go-faster/jx#Decoder.Base64) or
[jx.Decoder.Base64Append](https://pkg.go.dev/github.com/go-faster/jx#Decoder.Base64Append).

Same as encoding/json, base64.StdEncoding or [[RFC 4648](https://www.rfc-editor.org/rfc/rfc4648.html)].
```go
var e jx.Encoder
e.Base64([]byte("Hello"))
fmt.Println(e)

data, _ := jx.DecodeBytes(e.Bytes()).Base64()
fmt.Printf("%s", data)
// Output:
// "SGVsbG8="
// Hello
```

### Validate

Check that byte slice is valid json with [jx.Valid](https://pkg.go.dev/github.com/go-faster/jx#Valid):

```go
fmt.Println(jx.Valid([]byte(`{"field": "value"}`))) // true
fmt.Println(jx.Valid([]byte(`"Hello, world!"`)))    // true
fmt.Println(jx.Valid([]byte(`["foo"}`)))            // false
```

### Capture
The [jx.Decoder.Capture](https://pkg.go.dev/github.com/go-faster/jx#Decoder.Capture) method allows to unread everything is read in callback.
Useful for multi-pass parsing:
```go
d := jx.DecodeStr(`["foo", "bar", "baz"]`)
var elems int
// NB: Currently Capture does not support io.Reader, only buffers.
if err := d.Capture(func(d *jx.Decoder) error {
	// Everything decoded in this callback will be rolled back.
	return d.Arr(func(d *jx.Decoder) error {
		elems++
		return d.Skip()
	})
}); err != nil {
	panic(err)
}
// Decoder is rolled back to state before "Capture" call.
fmt.Println("Read", elems, "elements on first pass")
fmt.Println("Next element is", d.Next(), "again")

// Output:
// Read 3 elements on first pass
// Next element is array again
```

### ObjBytes

The `Decoder.ObjBytes` method tries not to allocate memory for keys, reusing existing buffer.
```go
d := DecodeStr(`{"id":1,"randomNumber":10}`)
d.ObjBytes(func(d *Decoder, key []byte) error {
    switch string(key) {
    case "id":
    case "randomNumber":
    }
    return d.Skip()
})
```

## Roadmap
- [ ] Rework and export `Any`
- [x] Support `Raw` for io.Reader
- [x] Support `Capture` for io.Reader
- [ ] Improve Num
  - Better validation on decoding
  - Support BigFloat and BigInt
  - Support equivalence check, like `eq(1.0, 1) == true`
- [ ] Add non-callback decoding of objects

## Non-goals
* Code generation for decoding or encoding
* Replacement for `encoding/json`
* Reflection or `interface{}` based encoding or decoding
* Support for json path or similar

This package should be kept as simple as possible and be used as
low-level foundation for high-level projects like code generator.

## License
MIT, same as jsoniter
