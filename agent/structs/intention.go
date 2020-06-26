package structs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/cache"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/go-multierror"
	"github.com/mitchellh/hashstructure"

	"golang.org/x/crypto/blake2b"
)

const (
	// IntentionDefaultNamespace is the default namespace value.
	// NOTE(mitchellh): This is only meant to be a temporary constant.
	// When namespaces are introduced, we should delete this constant and
	// fix up all the places where this was used with the proper namespace
	// value.
	IntentionDefaultNamespace = "default"
)

// Intention defines an intention for the Connect Service Graph. This defines
// the allowed or denied behavior of a connection between two services using
// Connect.
type Intention struct {
	// ID is the UUID-based ID for the intention, always generated by Consul.
	ID string

	// Description is a human-friendly description of this intention.
	// It is opaque to Consul and is only stored and transferred in API
	// requests.
	Description string

	// SourceNS, SourceName are the namespace and name, respectively, of
	// the source service. Either of these may be the wildcard "*", but only
	// the full value can be a wildcard. Partial wildcards are not allowed.
	// The source may also be a non-Consul service, as specified by SourceType.
	//
	// DestinationNS, DestinationName is the same, but for the destination
	// service. The same rules apply. The destination is always a Consul
	// service.
	SourceNS, SourceName           string
	DestinationNS, DestinationName string

	// SourceType is the type of the value for the source.
	SourceType IntentionSourceType

	// Action is whether this is an allowlist or denylist intention.
	Action IntentionAction

	// DefaultAddr is not used.
	// Deprecated: DefaultAddr is not used and may be removed in a future version.
	DefaultAddr string `bexpr:"-" codec:",omitempty"`
	// DefaultPort is not used.
	// Deprecated: DefaultPort is not used and may be removed in a future version.
	DefaultPort int `bexpr:"-" codec:",omitempty"`

	// Meta is arbitrary metadata associated with the intention. This is
	// opaque to Consul but is served in API responses.
	Meta map[string]string

	// Precedence is the order that the intention will be applied, with
	// larger numbers being applied first. This is a read-only field, on
	// any intention update it is updated.
	Precedence int

	// CreatedAt and UpdatedAt keep track of when this record was created
	// or modified.
	CreatedAt, UpdatedAt time.Time `mapstructure:"-" bexpr:"-"`

	// Hash of the contents of the intention
	//
	// This is needed mainly for replication purposes. When replicating from
	// one DC to another keeping the content Hash will allow us to detect
	// content changes more efficiently than checking every single field
	Hash []byte `bexpr:"-"`

	RaftIndex `bexpr:"-"`
}

func (t *Intention) Clone() *Intention {
	t2 := *t
	if t.Meta != nil {
		t2.Meta = make(map[string]string)
		for k, v := range t.Meta {
			t2.Meta[k] = v
		}
	}
	t2.Hash = nil
	return &t2
}

func (t *Intention) UnmarshalJSON(data []byte) (err error) {
	type Alias Intention
	aux := &struct {
		Hash                 string
		CreatedAt, UpdatedAt string // effectively `json:"-"` on Intention type

		*Alias
	}{
		Alias: (*Alias)(t),
	}
	if err = lib.UnmarshalJSON(data, &aux); err != nil {
		return err
	}

	if aux.Hash != "" {
		t.Hash = []byte(aux.Hash)
	}
	return nil
}

// SetHash calculates Intention.Hash from any mutable "content" fields.
//
// The Hash is primarily used for replication to determine if a token
// has changed and should be updated locally.
//
// TODO: move to agent/consul where it is called
func (x *Intention) SetHash() {
	hash, err := blake2b.New256(nil)
	if err != nil {
		panic(err)
	}

	// Write all the user set fields
	hash.Write([]byte(x.ID))
	hash.Write([]byte(x.Description))
	hash.Write([]byte(x.SourceNS))
	hash.Write([]byte(x.SourceName))
	hash.Write([]byte(x.DestinationNS))
	hash.Write([]byte(x.DestinationName))
	hash.Write([]byte(x.SourceType))
	hash.Write([]byte(x.Action))
	// hash.Write can not return an error, so the only way for binary.Write to
	// error is to pass it data with an invalid data type. Doing so would be a
	// programming error, so panic in that case.
	if err := binary.Write(hash, binary.LittleEndian, uint64(x.Precedence)); err != nil {
		panic(err)
	}

	// sort keys to ensure hash stability when meta is stored later
	var keys []string
	for k := range x.Meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		hash.Write([]byte(k))
		hash.Write([]byte(x.Meta[k]))
	}

	x.Hash = hash.Sum(nil)
}

// Validate returns an error if the intention is invalid for inserting
// or updating.
func (x *Intention) Validate() error {
	var result error

	// Empty values
	if x.SourceNS == "" {
		result = multierror.Append(result, fmt.Errorf("SourceNS must be set"))
	}
	if x.SourceName == "" {
		result = multierror.Append(result, fmt.Errorf("SourceName must be set"))
	}
	if x.DestinationNS == "" {
		result = multierror.Append(result, fmt.Errorf("DestinationNS must be set"))
	}
	if x.DestinationName == "" {
		result = multierror.Append(result, fmt.Errorf("DestinationName must be set"))
	}

	// Wildcard usage verification
	if x.SourceNS != WildcardSpecifier {
		if strings.Contains(x.SourceNS, WildcardSpecifier) {
			result = multierror.Append(result, fmt.Errorf(
				"SourceNS: wildcard character '*' cannot be used with partial values"))
		}
	}
	if x.SourceName != WildcardSpecifier {
		if strings.Contains(x.SourceName, WildcardSpecifier) {
			result = multierror.Append(result, fmt.Errorf(
				"SourceName: wildcard character '*' cannot be used with partial values"))
		}

		if x.SourceNS == WildcardSpecifier {
			result = multierror.Append(result, fmt.Errorf(
				"SourceName: exact value cannot follow wildcard namespace"))
		}
	}
	if x.DestinationNS != WildcardSpecifier {
		if strings.Contains(x.DestinationNS, WildcardSpecifier) {
			result = multierror.Append(result, fmt.Errorf(
				"DestinationNS: wildcard character '*' cannot be used with partial values"))
		}
	}
	if x.DestinationName != WildcardSpecifier {
		if strings.Contains(x.DestinationName, WildcardSpecifier) {
			result = multierror.Append(result, fmt.Errorf(
				"DestinationName: wildcard character '*' cannot be used with partial values"))
		}

		if x.DestinationNS == WildcardSpecifier {
			result = multierror.Append(result, fmt.Errorf(
				"DestinationName: exact value cannot follow wildcard namespace"))
		}
	}

	// Length of opaque values
	if len(x.Description) > metaValueMaxLength {
		result = multierror.Append(result, fmt.Errorf(
			"Description exceeds maximum length %d", metaValueMaxLength))
	}
	if len(x.Meta) > metaMaxKeyPairs {
		result = multierror.Append(result, fmt.Errorf(
			"Meta exceeds maximum element count %d", metaMaxKeyPairs))
	}
	for k, v := range x.Meta {
		if len(k) > metaKeyMaxLength {
			result = multierror.Append(result, fmt.Errorf(
				"Meta key %q exceeds maximum length %d", k, metaKeyMaxLength))
		}
		if len(v) > metaValueMaxLength {
			result = multierror.Append(result, fmt.Errorf(
				"Meta value for key %q exceeds maximum length %d", k, metaValueMaxLength))
		}
	}

	switch x.Action {
	case IntentionActionAllow, IntentionActionDeny:
	default:
		result = multierror.Append(result, fmt.Errorf(
			"Action must be set to 'allow' or 'deny'"))
	}

	switch x.SourceType {
	case IntentionSourceConsul:
	default:
		result = multierror.Append(result, fmt.Errorf(
			"SourceType must be set to 'consul'"))
	}

	return result
}

func (ixn *Intention) CanRead(authz acl.Authorizer) bool {
	if authz == nil {
		return true
	}
	var authzContext acl.AuthorizerContext

	// Read access on either end of the intention allows you to read the
	// complete intention. This is so that both ends can be aware of why
	// something does or does not work.

	if ixn.SourceName != "" {
		ixn.FillAuthzContext(&authzContext, false)
		if authz.IntentionRead(ixn.SourceName, &authzContext) == acl.Allow {
			return true
		}
	}

	if ixn.DestinationName != "" {
		ixn.FillAuthzContext(&authzContext, true)
		if authz.IntentionRead(ixn.DestinationName, &authzContext) == acl.Allow {
			return true
		}
	}

	return false
}

func (ixn *Intention) CanWrite(authz acl.Authorizer) bool {
	if authz == nil {
		return true
	}
	var authzContext acl.AuthorizerContext

	if ixn.DestinationName == "" {
		return false
	}

	ixn.FillAuthzContext(&authzContext, true)
	return authz.IntentionWrite(ixn.DestinationName, &authzContext) == acl.Allow
}

// UpdatePrecedence sets the Precedence value based on the fields of this
// structure.
func (x *Intention) UpdatePrecedence() {
	// Max maintains the maximum value that the precedence can be depending
	// on the number of exact values in the destination.
	var max int
	switch x.countExact(x.DestinationNS, x.DestinationName) {
	case 2:
		max = 9
	case 1:
		max = 6
	case 0:
		max = 3
	default:
		// This shouldn't be possible, just set it to zero
		x.Precedence = 0
		return
	}

	// Given the maximum, the exact value is determined based on the
	// number of source exact values.
	countSrc := x.countExact(x.SourceNS, x.SourceName)
	x.Precedence = max - (2 - countSrc)
}

// countExact counts the number of exact values (not wildcards) in
// the given namespace and name.
func (x *Intention) countExact(ns, n string) int {
	// If NS is wildcard, it must be zero since wildcards only follow exact
	if ns == WildcardSpecifier {
		return 0
	}

	// Same reasoning as above, a wildcard can only follow an exact value
	// and an exact value cannot follow a wildcard, so if name is a wildcard
	// we must have exactly one.
	if n == WildcardSpecifier {
		return 1
	}

	return 2
}

// String returns a human-friendly string for this intention.
func (x *Intention) String() string {
	return fmt.Sprintf("%s %s/%s => %s/%s (ID: %s, Precedence: %d)",
		strings.ToUpper(string(x.Action)),
		x.SourceNS, x.SourceName,
		x.DestinationNS, x.DestinationName,
		x.ID, x.Precedence)
}

// EstimateSize returns an estimate (in bytes) of the size of this structure when encoded.
func (x *Intention) EstimateSize() int {
	// 56 = 36 (uuid) + 16 (RaftIndex) + 4 (Precedence)
	size := 56 + len(x.Description) + len(x.SourceNS) + len(x.SourceName) + len(x.DestinationNS) +
		len(x.DestinationName) + len(x.SourceType) + len(x.Action)

	for k, v := range x.Meta {
		size += len(k) + len(v)
	}

	return size
}

// IntentionAction is the action that the intention represents. This
// can be "allow" or "deny".
type IntentionAction string

const (
	IntentionActionAllow IntentionAction = "allow"
	IntentionActionDeny  IntentionAction = "deny"
)

// IntentionSourceType is the type of the source within an intention.
type IntentionSourceType string

const (
	// IntentionSourceConsul is a service within the Consul catalog.
	IntentionSourceConsul IntentionSourceType = "consul"
)

// Intentions is a list of intentions.
type Intentions []*Intention

// IndexedIntentions represents a list of intentions for RPC responses.
type IndexedIntentions struct {
	Intentions Intentions
	QueryMeta
}

// IndexedIntentionMatches represents the list of matches for a match query.
type IndexedIntentionMatches struct {
	Matches []Intentions
	QueryMeta
}

// IntentionOp is the operation for a request related to intentions.
type IntentionOp string

const (
	IntentionOpCreate IntentionOp = "create"
	IntentionOpUpdate IntentionOp = "update"
	IntentionOpDelete IntentionOp = "delete"
)

// IntentionRequest is used to create, update, and delete intentions.
type IntentionRequest struct {
	// Datacenter is the target for this request.
	Datacenter string

	// Op is the type of operation being requested.
	Op IntentionOp

	// Intention is the intention.
	Intention *Intention

	// WriteRequest is a common struct containing ACL tokens and other
	// write-related common elements for requests.
	WriteRequest
}

// RequestDatacenter returns the datacenter for a given request.
func (q *IntentionRequest) RequestDatacenter() string {
	return q.Datacenter
}

// IntentionMatchType is the target for a match request. For example,
// matching by source will look for all intentions that match the given
// source value.
type IntentionMatchType string

const (
	IntentionMatchSource      IntentionMatchType = "source"
	IntentionMatchDestination IntentionMatchType = "destination"
)

// IntentionQueryRequest is used to query intentions.
type IntentionQueryRequest struct {
	// Datacenter is the target this request is intended for.
	Datacenter string

	// IntentionID is the ID of a specific intention.
	IntentionID string

	// Match is non-nil if we're performing a match query. A match will
	// find intentions that "match" the given parameters. A match includes
	// resolving wildcards.
	Match *IntentionQueryMatch

	// Check is non-nil if we're performing a test query. A test will
	// return allowed/deny based on an exact match.
	Check *IntentionQueryCheck

	// Exact is non-nil if we're performing a lookup of an intention by its
	// unique name instead of its ID.
	Exact *IntentionQueryExact

	// Options for queries
	QueryOptions
}

// RequestDatacenter returns the datacenter for a given request.
func (q *IntentionQueryRequest) RequestDatacenter() string {
	return q.Datacenter
}

// CacheInfo implements cache.Request
func (q *IntentionQueryRequest) CacheInfo() cache.RequestInfo {
	// We only support caching Match queries, so if Match isn't set,
	// then return an empty info object which will cause a pass-through
	// (and likely fail).
	if q.Match == nil {
		return cache.RequestInfo{}
	}

	info := cache.RequestInfo{
		Token:      q.Token,
		Datacenter: q.Datacenter,
		MinIndex:   q.MinQueryIndex,
		Timeout:    q.MaxQueryTime,
	}

	// Calculate the cache key via just hashing the Match struct. This
	// has been configured so things like ordering of entries has no
	// effect (via struct tags).
	v, err := hashstructure.Hash(q.Match, nil)
	if err == nil {
		// If there is an error, we don't set the key. A blank key forces
		// no cache for this request so the request is forwarded directly
		// to the server.
		info.Key = strconv.FormatUint(v, 16)
	}

	return info
}

// IntentionQueryMatch are the parameters for performing a match request
// against the state store.
type IntentionQueryMatch struct {
	Type    IntentionMatchType
	Entries []IntentionMatchEntry
}

// IntentionMatchEntry is a single entry for matching an intention.
type IntentionMatchEntry struct {
	Namespace string
	Name      string
}

// IntentionQueryCheck are the parameters for performing a test request.
type IntentionQueryCheck struct {
	// SourceNS, SourceName, DestinationNS, and DestinationName are the
	// source and namespace, respectively, for the test. These must be
	// exact values.
	SourceNS, SourceName           string
	DestinationNS, DestinationName string

	// SourceType is the type of the value for the source.
	SourceType IntentionSourceType
}

// GetACLPrefix returns the prefix to look up the ACL policy for this
// request, and a boolean noting whether the prefix is valid to check
// or not. You must check the ok value before using the prefix.
func (q *IntentionQueryCheck) GetACLPrefix() (string, bool) {
	return q.DestinationName, q.DestinationName != ""
}

// IntentionQueryCheckResponse is the response for a test request.
type IntentionQueryCheckResponse struct {
	Allowed bool
}

// IntentionQueryExact holds the parameters for performing a lookup of an
// intention by its unique name instead of its ID.
type IntentionQueryExact struct {
	SourceNS, SourceName           string
	DestinationNS, DestinationName string
}

// Validate is used to ensure all 4 parameters are specified.
func (q *IntentionQueryExact) Validate() error {
	var err error
	if q.SourceNS == "" {
		err = multierror.Append(err, errors.New("SourceNS is missing"))
	}
	if q.SourceName == "" {
		err = multierror.Append(err, errors.New("SourceName is missing"))
	}
	if q.DestinationNS == "" {
		err = multierror.Append(err, errors.New("DestinationNS is missing"))
	}
	if q.DestinationName == "" {
		err = multierror.Append(err, errors.New("DestinationName is missing"))
	}
	return err
}

// IntentionPrecedenceSorter takes a list of intentions and sorts them
// based on the match precedence rules for intentions. The intentions
// closer to the head of the list have higher precedence. i.e. index 0 has
// the highest precedence.
type IntentionPrecedenceSorter Intentions

func (s IntentionPrecedenceSorter) Len() int { return len(s) }
func (s IntentionPrecedenceSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s IntentionPrecedenceSorter) Less(i, j int) bool {
	a, b := s[i], s[j]
	if a.Precedence != b.Precedence {
		return a.Precedence > b.Precedence
	}

	// Tie break on lexicographic order of the 4-tuple in canonical form (SrcNS,
	// Src, DstNS, Dst). This is arbitrary but it keeps sorting deterministic
	// which is a nice property for consistency. It is arguably open to abuse if
	// implementations rely on this however by definition the order among
	// same-precedence rules is arbitrary and doesn't affect whether an allow or
	// deny rule is acted on since all applicable rules are checked.
	if a.SourceNS != b.SourceNS {
		return a.SourceNS < b.SourceNS
	}
	if a.SourceName != b.SourceName {
		return a.SourceName < b.SourceName
	}
	if a.DestinationNS != b.DestinationNS {
		return a.DestinationNS < b.DestinationNS
	}
	return a.DestinationName < b.DestinationName
}
