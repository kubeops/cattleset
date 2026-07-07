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

package v1

import "encoding/json"

// decodeSpecPatch unmarshals a sitePatch (a strategic merge patch over the
// rendered Deployment) and returns its top-level spec object. The second return
// is false when the patch is not a JSON object or carries no spec block; callers
// treat that as "nothing to check here", leaving malformed patches to fail at
// render time.
func decodeSpecPatch(raw []byte) (map[string]any, bool) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false
	}
	spec, ok := root["spec"].(map[string]any)
	if !ok {
		return nil, false
	}
	return spec, true
}

// patchedTemplateLabels walks spec.template.metadata.labels of a decoded spec
// patch, or nil when the path is absent.
func patchedTemplateLabels(spec map[string]any) map[string]any {
	tmpl, ok := spec["template"].(map[string]any)
	if !ok {
		return nil
	}
	meta, ok := tmpl["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	labels, ok := meta["labels"].(map[string]any)
	if !ok {
		return nil
	}
	return labels
}
