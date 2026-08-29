// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"encoding/json"
	"io"
)

func jsonDecoder(r io.Reader) *json.Decoder {
	return json.NewDecoder(r)
}
