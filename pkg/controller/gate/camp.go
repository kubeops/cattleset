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

package gate

import "strings"

// kubedbGroup is the API group of the KubeDB database kinds the camp table knows.
const kubedbGroup = "kubedb.com"

// campByKind is what "active" means for each engine. Only Postgres is documented
// and tested end to end; every other row is derived from the DC-DR fan-out matrix
// and has not been validated, so the gate treats everything but the leaderless
// engines restrictively, and an engine that is not listed is a single writer.
//
// Proxies and caches (PgBouncer, Pgpool, ProxySQL, Memcached) are transparent and
// should be gated on their backend. The controller cannot resolve a proxy's
// backend from a FailoverGroup member, so they are left as single writers: the
// restrictive reading.
var campByKind = map[string]Camp{
	"postgres":      CampSingleWriter,
	"mssqlserver":   CampSingleWriter,
	"oracle":        CampSingleWriter,
	"db2":           CampSingleWriter,
	"hanadb":        CampSingleWriter,
	"neo4j":         CampSingleWriter,
	"kafka":         CampAsyncMirror,
	"rabbitmq":      CampAsyncMirror,
	"elasticsearch": CampAsyncMirror,
	"solr":          CampAsyncMirror,
	"aerospike":     CampAsyncMirror,
	"hazelcast":     CampAsyncMirror,
	"ignite":        CampAsyncMirror,
	"singlestore":   CampAsyncMirror,
	"mongodb":       CampQuorumNative,
	"cassandra":     CampLeaderlessGeoNative,
	"weaviate":      CampLeaderlessGeoNative,
	"yugabytedb":    CampLeaderlessGeoNative,
}

// CampFor returns the camp of a data tier member. Unknown kinds, and any kind
// outside kubedb.com, are single writers.
func CampFor(apiGroup, kind string) Camp {
	if apiGroup == kubedbGroup {
		if c, ok := campByKind[strings.ToLower(kind)]; ok {
			return c
		}
	}
	return CampSingleWriter
}
