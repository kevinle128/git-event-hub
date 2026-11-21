// Package schema embeds the JSON Schema and the TypeScript types of the
// event contract, so tests and tools can read them.
package schema

import _ "embed"

// PREventV1 is schema/pr-event.v1.json.
//
//go:embed pr-event.v1.json
var PREventV1 []byte

// PREventV1Types is schema/pr-event.v1.d.ts.
//
//go:embed pr-event.v1.d.ts
var PREventV1Types []byte
