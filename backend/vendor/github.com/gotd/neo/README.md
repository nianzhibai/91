# neo

Package `neo` implements side effects (network, time) simulation for testing.

> Wake up, Neo...

Replace side effects with explicit dependencies so you can sleep at
night. Abuse time and network simulation in unit tests and reduce flaky,
complicated and long integration tests.

```go
package main

import (
	"fmt"
	"time"

	"github.com/gotd/neo"
)

func main() {
	// Set to current time.
	t := neo.NewTime(time.Now())

	// Travel to future.
	fmt.Println(t.Travel(time.Hour * 2).Format(time.RFC3339))
	// 2019-07-19T16:42:09+03:00

	// Back to past.
	fmt.Println(t.Travel(time.Hour * -2).Format(time.RFC3339))
	// 2019-07-19T14:42:09+03:00
}
```