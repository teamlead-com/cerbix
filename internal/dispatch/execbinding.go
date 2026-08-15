package dispatch

import (
	"crypto/sha256"
	"strconv"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// executionDTOVersion is the canonical encoding's OWN version, numbered independently of
// the envelope version and of the carrier generation, so the three can move apart without
// aliasing each other (func-secret-inventory §4.7, D-0160).
const executionDTOVersion = "1"

// ExecutionBodyDigest is SHA-256 over the canonical encoding of the credential-execution
// DTO: the members of the job that decide WHERE the credential goes, over what transport,
// and how many times it is transmitted.
//
// Why a digest at all: the identity parts of the AAD bind a credential to whose it is and
// when it was issued, but not to what the job asks it to do. Without this, anyone able to
// WRITE to a job carrier keeps a valid envelope, edits the target, and receives the
// plaintext credential at an endpoint of their choosing — the executor's authentication
// passes because nothing it verified changed.
//
// Why a dedicated DTO rather than the whole Monitor or the raw payload bytes: a JSON
// round-trip is not byte-stable, and a whole-struct hash breaks on fields that carry no
// execution meaning. The guarantee is deliberately credential USE and the remote side
// effects of a credentialed probe — not a general signature of the job. Excluded fields
// (name, tags, cadence, thresholds, delivery policy, the `*_ref` NAME) stay mutable in
// transit by design; widening this into full-job integrity is a separate decision.
//
// Both sides derive it from the body they hold: core from what it is about to publish, the
// executor from what it received, before any AEAD work.
func ExecutionBodyDigest(m domain.Monitor) ([32]byte, error) {
	parts, err := executionDTOParts(m)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(secret.CanonicalAAD(parts...)), nil
}

// executionDTOParts is the canonical encoding, specified to the byte because "canonical"
// without concrete values is an intention, not a format. It reuses secret.CanonicalAAD's
// framing — a uvarint part count followed by uvarint-length-prefixed parts — rather than
// inventing a second one, so no concatenation of different part lists can collide.
//
// Fixed order (never map iteration order):
//
//	[0] DTO version, decimal ASCII
//	[1] monitor type
//	[2] target
//	[3] timeout seconds, decimal ASCII
//	[4] retries, decimal ASCII
//	[5] condition count, then each condition in ARRAY order
//	[.] non-secret settings count, then each key and value, keys byte-wise sorted
//
// Integers are decimal ASCII with no padding, which removes width and endianness as
// questions instead of answering them; strings are raw UTF-8 under the length prefix.
// Condition ORDER is fixed for determinism, not because reordering an all-must-pass set
// changes the retry count — it changes which failure is reported first.
//
// Absent and explicitly-set-to-default encode identically: normalization has already
// materialized the canonical defaults, and a missing key reads as the same empty string an
// explicit blank would. That equivalence is the rule most likely to be broken by a
// well-meaning encoder, so the golden vectors pin it.
func executionDTOParts(m domain.Monitor) ([]string, error) {
	// The non-secret settings of the EFFECTIVE schema, from the one declarative registry —
	// so a new prober setting joins the digest by construction rather than by remembering.
	keys, err := domain.ExecutionBindingKeys(m.Type, m.Config)
	if err != nil {
		return nil, err
	}
	parts := make([]string, 0, 7+len(m.Conditions)+2*len(keys))
	parts = append(parts,
		executionDTOVersion,
		string(m.Type),
		m.Target,
		strconv.Itoa(m.TimeoutSeconds),
		strconv.Itoa(m.Retries),
		strconv.Itoa(len(m.Conditions)),
	)
	parts = append(parts, m.Conditions...)
	parts = append(parts, strconv.Itoa(len(keys)))
	for _, k := range keys {
		// Through the registry, so an ABSENT key resolves to its declared canonical value
		// and digests identically to a config that states that default explicitly. Reading
		// m.Config[k] directly made "absent" encode as "" and broke that equivalence for
		// every field whose default is not materialized into the stored config.
		value, err := domain.CanonicalSettingValue(m.Type, m.Config, k)
		if err != nil {
			return nil, err
		}
		parts = append(parts, k, value)
	}
	return parts, nil
}

// FieldSetDigest is SHA-256 over the canonical encoding of a credential field-NAME set.
// names must already be sorted; Seal and Open both derive it from the sorted names they
// hold, so truncating a multi-field envelope changes the AAD of every remaining field and
// fails authentication rather than being caught only by policy.
//
// Scope stated honestly: with today's single-field schemas the exact field-set POLICY in
// the structural gate is the operative protection and this adds no detection it does not
// already provide. It exists so a future multi-field envelope — a second credential slot, a
// client certificate — cannot be partially truncated into a still-valid job without a
// second security review.
func FieldSetDigest(names []string) [32]byte {
	parts := make([]string, 0, len(names)+1)
	parts = append(parts, strconv.Itoa(len(names)))
	parts = append(parts, names...)
	return sha256.Sum256(secret.CanonicalAAD(parts...))
}
