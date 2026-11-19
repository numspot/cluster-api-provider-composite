// Copyright 2026 Numspot.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"
)

func decodeAttachmentPin(raw string) *attachmentBinding {
	if raw == "" || len(raw) > maxProviderBindingsBytes || !utf8.ValidString(raw) {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var pin *attachmentBinding
	if err := decoder.Decode(&pin); err != nil || pin == nil {
		return nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil
	}
	// encoding/json otherwise accepts duplicate keys with last-value-wins
	// semantics. Check decoded keys, including escaped spellings and case aliases,
	// only after typed validation has bounded the accepted structure.
	if !uniquePinJSONValue(json.NewDecoder(strings.NewReader(raw))) {
		return nil
	}
	return pin
}

func uniquePinJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return true
	}
	if delimiter == '{' {
		return uniquePinJSONObject(decoder)
	}
	if delimiter != '[' {
		return false
	}
	for decoder.More() {
		if !uniquePinJSONValue(decoder) {
			return false
		}
	}
	closing, err := decoder.Token()
	return err == nil && closing == json.Delim(']')
}

func uniquePinJSONObject(decoder *json.Decoder) bool {
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		for _, previous := range keys {
			if strings.EqualFold(previous, key) {
				return false
			}
		}
		keys = append(keys, key)
		if !uniquePinJSONValue(decoder) {
			return false
		}
	}
	closing, err := decoder.Token()
	return err == nil && closing == json.Delim('}')
}
