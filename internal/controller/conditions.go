/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionReady is the single condition type both CRDs publish. Reasons are
// declared next to each CRD's status type in api/v1alpha1.
const ConditionReady = "Ready"

// setCondition upserts c into conditions and reports whether anything
// changed, so callers can fold it into their status-update dirty check.
// (meta.SetStatusCondition preserves lastTransitionTime when only the
// message/observedGeneration move.)
func setCondition(conditions *[]metav1.Condition, c metav1.Condition) bool {
	return meta.SetStatusCondition(conditions, c)
}
