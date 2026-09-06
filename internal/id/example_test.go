package id_test

import (
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/id"
)

// Each aggregate declares an unexported kind, an exported alias and a
// constructor. The kind is a phantom: it carries no data and only exists to
// make AccountID and OrderID different types.
type exampleAccountKind struct{}

type ExampleAccountID = id.ID[exampleAccountKind]

func NewExampleAccountID() ExampleAccountID { return id.New[exampleAccountKind]() }

func Example() {
	created := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	acct := id.NewAt[exampleAccountKind](created)

	fmt.Println(acct.Time().Format(time.RFC3339))
	fmt.Println(len(acct.String()), acct.IsZero())

	var zero ExampleAccountID
	fmt.Println(zero.IsZero(), zero)

	_, err := id.Parse[exampleAccountKind]("not-an-id")
	fmt.Println(err)

	// Output:
	// 2026-09-05T12:00:00Z
	// 36 false
	// true 00000000-0000-0000-0000-000000000000
	// id: not in canonical 36-character uuid form: length 9
}
