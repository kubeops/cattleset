/*
Copyright AppsCode Inc. and Contributors.

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

package cattleset

import "testing"

func TestPostureFlipped(t *testing.T) {
	tests := []struct {
		prior, holder string
		want          bool
	}{
		{"dc-a", "dc-b", true},  // realized failover
		{"dc-a", "dc-a", false}, // no change
		{"", "dc-a", false},     // first observation
		{"dc-a", "", false},     // lost the Lease (not a flip, fail closed handles it)
		{"", "", false},         // unknown throughout
	}
	for _, tc := range tests {
		if got := postureFlipped(tc.prior, tc.holder); got != tc.want {
			t.Errorf("postureFlipped(%q,%q) = %v, want %v", tc.prior, tc.holder, got, tc.want)
		}
	}
}
