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
	"fmt"

	infrav1 "github.com/numspot/cluster-api-provider-composite/api/v1beta2"
)

const pinDiagnosticOmitted = "<omitted>"

// Only constructors below build these messages, from fixed text and bounded
// allowlisted identity. Never promote a native error's Error() text to a message.
// The embedded cause preserves errors.Is through status/log diagnostic wrappers.
type pinDiagnosticError struct{ diagnosticError }

func providerLocalPinDiagnostic() error {
	return &pinDiagnosticError{diagnosticError{
		message: "Provider-local binding pin is missing, invalid or conflicts with the CompositeCluster attachment record; " +
			"compare both controller-owned pins with verified backups. Annotation edits and automatic rebinding are unsupported.",
		cause: errProfilePinMissing,
	}}
}

func providerIdentityDiagnostic(p *infrav1.CompositeProvider, previous *attachmentBinding) error {
	identity := pinDiagnosticLabel(p.Namespace) + "/" + pinDiagnosticLabel(p.Name)
	pinned, live := pinDiagnosticToken(string(previous.UID)), pinDiagnosticToken(string(p.UID))
	message := fmt.Sprintf("CompositeProvider %s UID mismatch with the CompositeCluster attachment record: pinned UID=%s, live UID=%s. "+
		"The attachment name remains reserved; automatic replacement is unsupported.", identity, pinned, live)
	if previous.Retired {
		message = fmt.Sprintf(
			"Attachment name %s is reserved by a retired CompositeCluster attachment record: pinned provider UID=%s, live provider UID=%s. "+
				"Use a new attachment name subject to native-name/canonical constraints and retained-history limits; automatic replacement is unsupported.",
			identity,
			pinned,
			live,
		)
	}
	return &pinDiagnosticError{diagnosticError{message: message, cause: errProfileMismatch}}
}

func profileRevisionDiagnostic(previous, live *providerBinding, catalog *infrav1.CompositeProviderTemplate) error {
	switch {
	case previous.ProfileUID != live.ProfileUID:
		message := fmt.Sprintf("Profile CompositeProviderTemplate %s UID mismatch: pinned UID=%s, live UID=%s. "+
			"Existing infrastructure is retained; automatic rebinding is unsupported.", pinDiagnosticToken(catalog.Name),
			pinDiagnosticToken(string(previous.ProfileUID)), pinDiagnosticToken(string(live.ProfileUID)))
		return &pinDiagnosticError{diagnosticError{message: message, cause: errProfileMismatch}}
	case previous.InfrastructureRevision != live.InfrastructureRevision:
		return sourceRevisionDiagnostic(
			"Infrastructure",
			catalog.Spec.InfrastructureTemplateRef,
			previous.InfrastructureRevision,
			live.InfrastructureRevision,
		)
	case previous.BootstrapRevision != live.BootstrapRevision:
		return sourceRevisionDiagnostic("Bootstrap", catalog.Spec.BootstrapTemplateRef, previous.BootstrapRevision, live.BootstrapRevision)
	default:
		return errProfileMismatch
	}
}

func sourceRevisionDiagnostic(label string, ref infrav1.ProviderTemplateReference, pinned, live sourceRevision) error {
	message := fmt.Sprintf("%s source %s %s %s/%s revision mismatch: pinned UID=%s generation=%d, live UID=%s generation=%d. "+
		"Existing infrastructure is retained; use immutable named source revisions for new attachments. Annotation edits and automatic rebinding are unsupported.",
		label, pinDiagnosticAPIVersion(ref.APIVersion), pinDiagnosticLabel(ref.Kind), pinDiagnosticLabel(ref.Namespace), pinDiagnosticToken(ref.Name),
		pinDiagnosticToken(string(pinned.UID)), pinned.Generation, pinDiagnosticToken(string(live.UID)), live.Generation)
	return &pinDiagnosticError{diagnosticError{message: message, cause: errProfileMismatch}}
}

// Identity inputs include durable annotations and source metadata, not just
// admitted Kubernetes fields. Omit unsafe or overlong values rather than quote,
// truncate or copy arbitrary content; keep both revisions in the bounded message.
func pinDiagnosticToken(value string) string {
	if value == "" || len(value) > 128 {
		return pinDiagnosticOmitted
	}
	for _, c := range value {
		if !pinDiagnosticCharacter(c) {
			return pinDiagnosticOmitted
		}
	}
	return value
}

func pinDiagnosticLabel(value string) string {
	if len(value) > 63 {
		return pinDiagnosticOmitted
	}
	return pinDiagnosticToken(value)
}

func pinDiagnosticCharacter(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_'
}

func pinDiagnosticAPIVersion(value string) string {
	if value == "" || len(value) > 128 {
		return pinDiagnosticOmitted
	}
	for _, c := range value {
		if c != '/' && !pinDiagnosticCharacter(c) {
			return pinDiagnosticOmitted
		}
	}
	return value
}
